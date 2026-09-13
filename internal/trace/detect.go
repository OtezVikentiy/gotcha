package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
)

// Виды находок; те же значения лежат в колонке perf_issues.kind.
const (
	KindNPlusOne    = notify.KindNPlusOne
	KindSlowDBQuery = notify.KindSlowDBQuery
	KindHTTPFlood   = notify.KindHTTPFlood
)

const (
	slowDBSharePercent    = 30
	httpSequentialPercent = 80
	maxEvidenceSpanIDs    = 10
	maxEvidenceURLs       = 10

	// Делитель абсолютного порога: доля от транзакции сама по себе не проблема.
	slowDBShareFloorDivisor = 5

	maxFindingsPerTransaction = 20

	// Во столько раз количество спанов должно превышать NPlusOneMin, чтобы N+1 сработал
	// без пола по времени — у SDK со сломанными часами суммарное время группы нулевое.
	zeroClockCountFactor = 2
)

// `db.sql.query`, `db`, `db.redis` — сопоставление по префиксу, а не равенству.
// `http.client` — исходящие вызовы, `http.server` — сама транзакция.
const (
	opPrefixDB         = "db"
	opPrefixHTTPClient = "http.client"
)

type DetectorConfig struct {
	NPlusOneMin        int `json:"n_plus_one_min"`          // сколько одинаковых db-спанов под одним родителем — уже N+1
	NPlusOneMinTotalMs int `json:"n_plus_one_min_total_ms"` // и сколько миллисекунд они суммарно должны съесть
	SlowDBMs           int `json:"slow_db_ms"`              // db-спан дольше этого — медленный
	HTTPFloodMin       int `json:"http_flood_min"`          // столько исходящих HTTP-вызовов в транзакции — лавина
}

func DefaultDetectorConfig() DetectorConfig {
	return DetectorConfig{NPlusOneMin: 5, NPlusOneMinTotalMs: 20, SlowDBMs: 500, HTTPFloodMin: 10}
}

// Нулевой порог означал бы «срабатывать всегда» — детектор хуже отсутствующего.
func (c DetectorConfig) withDefaults() DetectorConfig {
	def := DefaultDetectorConfig()
	if c.NPlusOneMin <= 0 {
		c.NPlusOneMin = def.NPlusOneMin
	}
	if c.NPlusOneMinTotalMs <= 0 {
		c.NPlusOneMinTotalMs = def.NPlusOneMinTotalMs
	}
	if c.SlowDBMs <= 0 {
		c.SlowDBMs = def.SlowDBMs
	}
	if c.HTTPFloodMin <= 0 {
		c.HTTPFloodMin = def.HTTPFloodMin
	}
	return c
}

// При ошибке разбора возвращаются дефолты вместе с ошибкой — вызывающий может продолжить на них.
func ConfigFromJSON(raw []byte) (DetectorConfig, error) {
	if len(raw) == 0 {
		return DefaultDetectorConfig(), nil
	}
	var cfg DetectorConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return DefaultDetectorConfig(), fmt.Errorf("perf detector config: %w", err)
	}
	return cfg.withDefaults(), nil
}

type Finding struct {
	Kind        string         // KindNPlusOne | KindSlowDBQuery | KindHTTPFlood
	Culprit     string         // имя транзакции
	Fingerprint string         // hash(kind, НОРМАЛИЗОВАННЫЙ culprit, нормализованное описание) — БЕЗ project_id, его добавит upsert
	Description string         // нормализованное описание
	Evidence    map[string]any // count, total_ms, span_ids (до 10) и поля, специфичные для вида
}

// Функция ЧИСТАЯ: никакой БД, состояния или часов — та же транзакция всегда даёт те же
// находки в том же порядке, отсюда стабильность фингерпринтов.
func Detect(t Transaction, cfg DetectorConfig) []Finding {
	cfg = cfg.withDefaults()

	// culprit="" склеил бы в одну проблему все безымянные транзакции (у OTLP-спана имя
	// может отсутствовать) — молчание честнее: транзакция всё равно видна в трейсах.
	if fingerprintCulprit(t.Name) == "" {
		slog.Warn("perf detection skipped: transaction has no name", "trace_id", t.TraceID)
		return nil
	}

	var findings []Finding
	findings = append(findings, detectNPlusOne(t, cfg)...)
	findings = append(findings, detectSlowDBQueries(t, cfg)...)
	findings = append(findings, detectHTTPFlood(t, cfg)...)
	return capFindings(mergeByFingerprint(findings), t)
}

// N+1 группируется по (родитель, запрос), а фингерпринт — по (вид, транзакция, запрос):
// один запрос под ДВУМЯ родителями даёт две находки с ОДНИМ фингерпринтом.
func mergeByFingerprint(fs []Finding) []Finding {
	if len(fs) < 2 {
		return fs
	}
	out := make([]Finding, 0, len(fs))
	at := make(map[string]int, len(fs))
	for _, f := range fs {
		i, ok := at[f.Fingerprint]
		if !ok {
			at[f.Fingerprint] = len(out)
			out = append(out, f)
			continue
		}
		mergeEvidence(out[i].Evidence, f.Evidence)
	}
	return out
}

// Прочие поля (parent_op, sequential, urls) остаются от первой находки, dst.
func mergeEvidence(dst, src map[string]any) {
	if c, ok := src["count"].(int); ok {
		prev, _ := dst["count"].(int)
		dst["count"] = prev + c
	}
	if us, ok := src["total_us"].(int64); ok {
		prev, _ := dst["total_us"].(int64)
		dst["total_us"] = prev + us
	}
	if us, ok := src["max_us"].(int64); ok {
		if prev, _ := dst["max_us"].(int64); us > prev {
			dst["max_us"] = us
		}
	}
	ids, _ := dst["span_ids"].([]string)
	for _, id := range srcSpanIDs(src) {
		if len(ids) >= maxEvidenceSpanIDs {
			break
		}
		ids = append(ids, id)
	}
	dst["span_ids"] = ids
}

func srcSpanIDs(ev map[string]any) []string {
	ids, _ := ev["span_ids"].([]string)
	return ids
}

// Отбор по убыванию суммарного времени, при равенстве — по фингерпринту: иначе результат
// зависел бы от порядка обхода карты. Отброшенное логируется.
func capFindings(fs []Finding, t Transaction) []Finding {
	if len(fs) <= maxFindingsPerTransaction {
		return fs
	}
	sorted := make([]Finding, len(fs))
	copy(sorted, fs)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, _ := sorted[i].Evidence["total_us"].(int64)
		b, _ := sorted[j].Evidence["total_us"].(int64)
		if a != b {
			return a > b
		}
		return sorted[i].Fingerprint < sorted[j].Fingerprint
	})
	slog.Warn("perf findings capped, weakest dropped",
		"trace_id", t.TraceID, "transaction", t.Name,
		"found", len(fs), "kept", maxFindingsPerTransaction,
		"dropped", len(fs)-maxFindingsPerTransaction)
	return sorted[:maxFindingsPerTransaction]
}

type spanGroup struct {
	desc     string
	parentOp string
	count    int
	totalUS  uint64
	maxUS    uint64
	spanIDs  []string
}

func (g *spanGroup) add(s Span) {
	us := uint64(s.DurationUS())
	g.count++
	g.totalUS += us
	if us > g.maxUS {
		g.maxUS = us
	}
	if len(g.spanIDs) < maxEvidenceSpanIDs {
		g.spanIDs = append(g.spanIDs, s.SpanID)
	}
}

// Ключи хранятся отдельным слайсом order, чтобы обход не зависел от порядка карты.
type groupIndex struct {
	order  []string
	groups map[string]*spanGroup
}

func newGroupIndex() *groupIndex {
	return &groupIndex{groups: make(map[string]*spanGroup)}
}

func (gi *groupIndex) get(key, desc, parentOp string) *spanGroup {
	g, ok := gi.groups[key]
	if !ok {
		g = &spanGroup{desc: desc, parentOp: parentOp}
		gi.groups[key] = g
		gi.order = append(gi.order, key)
	}
	return g
}

func (gi *groupIndex) each(fn func(g *spanGroup)) {
	for _, key := range gi.order {
		fn(gi.groups[key])
	}
}

// Родитель входит в ключ группы: разные родители — разные места кода, не N+1.
func detectNPlusOne(t Transaction, cfg DetectorConfig) []Finding {
	parentOps := spanOps(t)

	gi := newGroupIndex()
	for _, s := range t.Spans {
		if !hasOpPrefix(s.Op, opPrefixDB) {
			continue
		}
		desc := NormalizeDescription(s.Op, s.Description)
		gi.get(s.ParentSpanID+"\x00"+desc, desc, parentOps[s.ParentSpanID]).add(s)
	}

	minTotalUS := uint64(cfg.NPlusOneMinTotalMs) * 1000

	var out []Finding
	gi.each(func(g *spanGroup) {
		if g.count < cfg.NPlusOneMin {
			return
		}
		// totalUS == 0 значит сломанные часы SDK, не «дёшево» — пол по времени не применяется,
		// если группа вдвое многочисленнее порога.
		zeroClock := g.totalUS == 0 && g.count >= zeroClockCountFactor*cfg.NPlusOneMin
		if g.totalUS < minTotalUS && !zeroClock {
			return
		}
		ev := baseEvidence(g)
		ev["parent_op"] = g.parentOp
		out = append(out, newFinding(KindNPlusOne, t.Name, g.desc, ev))
	})
	return out
}

// Пол обязателен: правило «доля» без него срабатывает на каждом простом эндпойнте, где
// один запрос — почти вся работа. Пол выводится из SlowDBMs, а не задан отдельным числом.
func detectSlowDBQueries(t Transaction, cfg DetectorConfig) []Finding {
	txUS := uint64(t.DurationUS())
	thresholdUS := uint64(cfg.SlowDBMs) * 1000
	floorUS := thresholdUS / slowDBShareFloorDivisor

	gi := newGroupIndex()
	for _, s := range t.Spans {
		if !hasOpPrefix(s.Op, opPrefixDB) {
			continue
		}
		us := uint64(s.DurationUS())
		// Доля от транзакции считается, только если длительность известна:
		// транзакции с нулевой длительностью присылают SDK со сломанными часами.
		bigShare := txUS > 0 && us > floorUS && us*100 > txUS*slowDBSharePercent
		if us <= thresholdUS && !bigShare {
			continue
		}
		desc := NormalizeDescription(s.Op, s.Description)
		gi.get(desc, desc, "").add(s)
	}

	var out []Finding
	gi.each(func(g *spanGroup) {
		ev := baseEvidence(g)
		ev["max_us"] = int64(g.maxUS)
		out = append(out, newFinding(KindSlowDBQuery, t.Name, g.desc, ev))
	})
	return out
}

// гейтов два, хватает любого: isSequential или ограниченный параллелизм пула (HTTPFloodMin).
// фингерпринт держится на (kind, culprit): адреса разные, в evidence кладутся отдельно.
func detectHTTPFlood(t Transaction, cfg DetectorConfig) []Finding {
	g := &spanGroup{}
	var calls []Span
	urls := make([]string, 0, maxEvidenceURLs)
	seen := make(map[string]bool)
	for _, s := range t.Spans {
		if !hasOpPrefix(s.Op, opPrefixHTTPClient) {
			continue
		}
		g.add(s)
		calls = append(calls, s)
		if u := NormalizeDescription(s.Op, s.Description); u != "" && !seen[u] {
			seen[u] = true
			if len(urls) < maxEvidenceURLs {
				urls = append(urls, u)
			}
		}
	}
	if g.count < cfg.HTTPFloodMin {
		return nil
	}
	wallUS := mergedWallUS(calls)
	concurrency := maxConcurrency(calls)
	if !isSequential(g.totalUS, wallUS) && !isBoundedWaterfall(g.count, concurrency, cfg.HTTPFloodMin) {
		return nil
	}

	ev := baseEvidence(g)
	// измеренная доля стенного времени, не флаг «sequential»: флаг после гейта всегда true.
	// доля показывает, насколько вызовы последовательны (100% — цепочка, ~50% — водопад).
	ev["sequential_pct"] = sequentialPct(g.totalUS, wallUS)
	ev["max_concurrency"] = concurrency
	// Это op САМОЙ транзакции (http.server), а не родителя вызовов: у лавины
	// родителя нет — вызовы принадлежат эндпойнту целиком.
	ev["transaction_op"] = t.Op
	ev["urls"] = urls

	return []Finding{newFinding(KindHTTPFlood, t.Name, "", ev)}
}

// Сравнивать сумму нужно со стенным временем вызовов, не длительностью транзакции —
// параллельный веер даёт там сотни процентов и уверенно проходит любой порог.
func isSequential(totalUS, wallUS uint64) bool {
	if totalUS == 0 || wallUS == 0 {
		return false
	}
	return wallUS*100 >= totalUS*httpSequentialPercent
}

// Та же величина, что сравнивает isSequential, но числом: 100% — строгая цепочка.
func sequentialPct(totalUS, wallUS uint64) int {
	if totalUS == 0 {
		return 0
	}
	return int(wallUS * 100 / totalUS)
}

// count/concurrency — число волн, которые эндпойнт отстоял в очереди; полностью
// параллельный веер даёт глубину 1 и молчит.
func isBoundedWaterfall(count, concurrency, floodMin int) bool {
	if concurrency <= 0 {
		return false // часы SDK сломаны: параллелизм не измерить
	}
	return count/concurrency >= floodMin
}

// Спаны с нулевой длительностью (сломанные часы SDK) не участвуют — как и в mergedWallUS.
func maxConcurrency(spans []Span) int {
	type edge struct {
		at    time.Time
		delta int
	}
	edges := make([]edge, 0, 2*len(spans))
	for _, s := range spans {
		if s.DurationUS() == 0 {
			continue
		}
		edges = append(edges, edge{s.Start, 1}, edge{s.End, -1})
	}
	if len(edges) == 0 {
		return 0
	}
	// При равных отметках сначала закрываем (-1), потом открываем (+1): вызов,
	// начавшийся ровно в миг завершения предыдущего, шёл ПОСЛЕ него, а не вместе.
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].at.Equal(edges[j].at) {
			return edges[i].delta < edges[j].delta
		}
		return edges[i].at.Before(edges[j].at)
	})

	cur, peak := 0, 0
	for _, e := range edges {
		cur += e.delta
		if cur > peak {
			peak = cur
		}
	}
	return peak
}

// Интервалы [Start, End) сливаются, пересечения не считаются дважды.
func mergedWallUS(spans []Span) uint64 {
	type iv struct{ start, end time.Time }
	ivs := make([]iv, 0, len(spans))
	for _, s := range spans {
		if s.DurationUS() == 0 { // сломанные часы SDK: End <= Start
			continue
		}
		ivs = append(ivs, iv{s.Start, s.End})
	}
	if len(ivs) == 0 {
		return 0
	}
	sort.Slice(ivs, func(i, j int) bool {
		if ivs[i].start.Equal(ivs[j].start) {
			return ivs[i].end.Before(ivs[j].end)
		}
		return ivs[i].start.Before(ivs[j].start)
	})

	var total time.Duration
	cur := ivs[0]
	for _, v := range ivs[1:] {
		if v.start.After(cur.end) { // разрыв: предыдущий отрезок закрыт
			total += cur.end.Sub(cur.start)
			cur = v
			continue
		}
		if v.end.After(cur.end) { // перекрытие: расширяем текущий отрезок
			cur.end = v.end
		}
	}
	total += cur.end.Sub(cur.start)

	us := total.Microseconds()
	if us < 0 {
		return 0
	}
	return uint64(us)
}

// Время в МИКРОсекундах: Redis-спаны субмиллисекундные, в мс total ушёл бы в 0.
func baseEvidence(g *spanGroup) map[string]any {
	ids := g.spanIDs
	if ids == nil {
		ids = []string{}
	}
	return map[string]any{
		"count":    g.count,
		"total_us": int64(g.totalUS),
		"span_ids": ids,
	}
}

// В Culprit остаётся сырое имя транзакции (читает человек), в фингерпринт идёт
// нормализованное — иначе нешаблонизированные маршруты плодят проблему на каждый запрос.
func newFinding(kind, culprit, desc string, ev map[string]any) Finding {
	return Finding{
		Kind:        kind,
		Culprit:     culprit,
		Fingerprint: fingerprintOf(kind, fingerprintCulprit(culprit), desc),
		Description: capRunes(desc, maxNormalizedDescription+64),
		Evidence:    ev,
	}
}

// `GET /users/42` → `GET /users/{id}`. Пустое имя остаётся пустым — Detect такие
// транзакции пропускает целиком, а не склеивает в общую проблему.
func fingerprintCulprit(name string) string {
	return strings.TrimSpace(NormalizeURL(name))
}

// project_id сюда НЕ входит: он часть уникального ключа perf_issues, добавляется upsert-ом.
func fingerprintOf(kind, culprit, desc string) string {
	h := sha256.New()
	// \x00 — разделитель: без него ("ab","c") совпало бы с ("a","bc").
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(culprit))
	h.Write([]byte{0})
	h.Write([]byte(desc))
	return hex.EncodeToString(h.Sum(nil))
}

// `db` матчит `db.sql.query` и `db.redis`, но не `dbutil.something`.
func hasOpPrefix(op, prefix string) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	if !strings.HasPrefix(op, prefix) {
		return false
	}
	return len(op) == len(prefix) || op[len(prefix)] == '.'
}

// Включает корневой спан транзакции: родителем db-спана бывает и сама транзакция.
func spanOps(t Transaction) map[string]string {
	ops := make(map[string]string, len(t.Spans)+1)
	ops[t.SpanID] = t.Op
	for _, s := range t.Spans {
		ops[s.SpanID] = s.Op
	}
	return ops
}
