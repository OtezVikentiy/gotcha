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
)

// Виды находок; те же значения лежат в колонке perf_issues.kind.
const (
	KindNPlusOne    = "n_plus_one"
	KindSlowDBQuery = "slow_db_query"
	KindHTTPFlood   = "http_flood"
)

// Пороги-константы, которые не настраиваются (в отличие от DetectorConfig):
//
//   - slowDBSharePercent: спан, съевший столько процентов транзакции, подозрителен
//     — 300мс в запросе на 400мс это проблема, даже если SlowDBMs=500. Но ДОЛЯ
//     сама по себе ничего не значит: у здорового простого эндпойнта один запрос и
//     есть почти вся его работа. Поэтому доля работает только выше пола (см.
//     slowDBShareFloorDivisor);
//   - httpSequentialPercent: доля СТЕННОГО времени, реально закрытого исходящими
//     вызовами, от их суммарной длительности. Последовательные вызовы идут друг
//     за другом (сумма ≈ стенное время), параллельные перекрываются (сумма
//     сильно больше). Это не украшение evidence, а ГЕЙТ http_flood: уже
//     распараллеленный веер — не проблема (см. detectHTTPFlood);
//   - maxEvidenceSpanIDs / maxEvidenceURLs: сколько id спанов и адресов кладём в
//     evidence (оно уезжает в JSONB, класть все 500 спанов N+1 незачем).
const (
	slowDBSharePercent    = 30
	httpSequentialPercent = 80
	maxEvidenceSpanIDs    = 10
	maxEvidenceURLs       = 10

	// slowDBShareFloorDivisor — делитель абсолютного порога: доля от транзакции
	// сама по себе проблемой не является. Пол = SlowDBMs/5 (100мс на дефолтных
	// 500мс).
	slowDBShareFloorDivisor = 5

	// maxFindingsPerTransaction — сколько находок отдаёт Detect максимум.
	maxFindingsPerTransaction = 20

	// zeroClockCountFactor — во сколько раз количество спанов в группе должно
	// превышать NPlusOneMin, чтобы N+1 сработал БЕЗ пола по времени (у SDK со
	// сломанными часами суммарное время группы нулевое, см. detectNPlusOne).
	zeroClockCountFactor = 2
)

// Префиксы op спанов. В природе встречаются `db.sql.query`, `db`, `db.redis`
// (Sentry SDK и наш OTLP-маппинг, см. internal/ingest/otlp.go), поэтому
// сопоставление идёт по префиксу, а не по точному равенству. `http.client` —
// именно исходящие вызовы: `http.server` это сама транзакция.
const (
	opPrefixDB         = "db"
	opPrefixHTTPClient = "http.client"
)

// DetectorConfig — пороги детекторов, приезжают из projects.perf_detector_config.
type DetectorConfig struct {
	NPlusOneMin        int `json:"n_plus_one_min"`          // сколько одинаковых db-спанов под одним родителем — уже N+1
	NPlusOneMinTotalMs int `json:"n_plus_one_min_total_ms"` // и сколько миллисекунд они суммарно должны съесть
	SlowDBMs           int `json:"slow_db_ms"`              // db-спан дольше этого — медленный
	HTTPFloodMin       int `json:"http_flood_min"`          // столько исходящих HTTP-вызовов в транзакции — лавина
}

// DefaultDetectorConfig — дефолты из спеки (§5) плюс пол по времени у N+1.
//
// NPlusOneMinTotalMs=20 — осознанный компромисс. Одного счётчика мало: 20
// обращений в Redis по 0.2мс — это 4мс на весь запрос, и будить из-за них
// человека нельзя (проблема, которую нельзя починить с выгодой, — не проблема).
// У Sentry этот пол равен 100мс, но он же гасит и настоящий кеш-цикл из восьми
// запросов по 0.3мс. 20мс — середина: заметная для эндпойнта работа (5% от
// 400мс SLA), но ниже, чем цена одного лишнего похода в БД по сети.
func DefaultDetectorConfig() DetectorConfig {
	return DetectorConfig{NPlusOneMin: 5, NPlusOneMinTotalMs: 20, SlowDBMs: 500, HTTPFloodMin: 10}
}

// withDefaults подставляет дефолты вместо неположительных порогов: нулевой
// порог означал бы «срабатывать всегда», а детектор, который срабатывает
// всегда, хуже отсутствующего.
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

// ConfigFromJSON парсит projects.perf_detector_config. Пустой/nil вход — не
// ошибка (колонка по умолчанию '{}'), отсутствующие и неположительные поля
// заменяются дефолтами. При ошибке разбора возвращаются дефолты вместе с
// ошибкой: вызывающий может продолжить детекцию на дефолтах.
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

// Finding — найденная в транзакции проблема производительности. Дальше её
// подхватывает upsert в perf_issues (см. perfissue.go).
type Finding struct {
	Kind        string         // KindNPlusOne | KindSlowDBQuery | KindHTTPFlood
	Title       string         // человекочитаемый заголовок
	Culprit     string         // имя транзакции
	Fingerprint string         // hash(kind, НОРМАЛИЗОВАННЫЙ culprit, нормализованное описание) — БЕЗ project_id, его добавит upsert
	Description string         // нормализованное описание
	Evidence    map[string]any // count, total_ms, span_ids (до 10) и поля, специфичные для вида
}

// Detect ищет в транзакции проблемы производительности: N+1 запросов,
// медленные запросы к БД и лавину исходящих HTTP-вызовов.
//
// Функция ЧИСТАЯ: никакой БД, никакого состояния, никаких часов — та же
// транзакция на любой реплике даёт те же находки в том же порядке. Отсюда и
// стабильность фингерпринтов. Единственный побочный эффект — warn-лог о
// сработавшем капе находок (см. capFindings): на результат он не влияет.
//
// Детекция идёт по спанам ОДНОЙ транзакции — так же устроен Sentry. Для OTLP
// это best-effort: видны спаны, приехавшие в одном запросе; собирать трейс
// между запросами мы не пытаемся.
//
// Порядок находок: сначала N+1, потом медленные запросы, потом лавина HTTP;
// внутри вида — в порядке первого появления группы среди спанов. Если находок
// больше maxFindingsPerTransaction, остаются только самые тяжёлые (см.
// capFindings), и порядок становится «по убыванию времени».
//
// Находки с одинаковым фингерпринтом склеиваются в одну (см. mergeByFingerprint):
// одна транзакция — не больше одной записи на проблему.
func Detect(t Transaction, cfg DetectorConfig) []Finding {
	cfg = cfg.withDefaults()

	// Проблема производительности принадлежит ЭНДПОЙНТУ, и без его имени её не к
	// чему привязать: culprit="" склеил бы в одну проблему все безымянные
	// транзакции (у OTLP-спана имя может отсутствовать) — несвязанные куски кода
	// оказались бы одной строкой perf_issues с общим счётчиком. Молчание здесь
	// честнее: транзакция всё равно записана в CH, и её видно в трейсах.
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

// mergeByFingerprint склеивает находки с одинаковым фингерпринтом: группировка
// N+1 идёт по (родитель, запрос), а фингерпринт — по (вид, транзакция, запрос),
// поэтому один и тот же запрос, зациклённый под ДВУМЯ родителями, даёт две
// находки с ОДНИМ фингерпринтом. Без склейки Record позвался бы дважды за одну
// транзакцию: count вырос бы на 2, а evidence второй группы затёр бы первую
// (span_ids первой группы потерялись бы вместе с ней).
//
// Порядок первого появления сохраняется, счётчики и время суммируются, span_ids
// объединяются до общего капа.
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

// mergeEvidence вливает src в dst: count и total_us складываются, max_us берётся
// максимальный, span_ids объединяются до капа. Прочие поля (parent_op,
// sequential, urls) остаются от первой находки — она первая и по порядку спанов.
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

// capFindings оставляет не больше maxFindingsPerTransaction находок. Без капа
// одна транзакция (до 1000 спанов) с сотней разных медленных запросов породила
// бы сотню строк perf_issues за раз. Рассылку сверх лимита гасит троттлинг
// (см. OutboxNotifier.claimAlert), но строки в PG он не отменяет — их и режет
// кап.
//
// Отбор — по убыванию суммарного времени (при равенстве — по фингерпринту,
// чтобы результат не зависел от порядка карты): то, что дороже, важнее.
// Отброшенное логируется: молчаливый кап читался бы как «мы нашли всё».
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

// spanGroup — накопитель по группе спанов. Ключи групп хранятся отдельным
// слайсом (см. groupIndex), чтобы порядок обхода не зависел от карты.
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

// groupIndex — группы спанов в порядке первого появления.
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

// detectNPlusOne группирует db-спаны по (ParentSpanID, нормализованное
// описание): N одинаковых запросов, выпущенных из ОДНОГО места (цикл по
// результатам родительского запроса) — это и есть N+1. Разные родители — это
// не N+1, а разные места кода, поэтому родитель входит в ключ группы: без него
// детектор начал бы находить проблемы там, где их нет.
//
// Условий ДВА: и количество (NPlusOneMin), и суммарное время
// (NPlusOneMinTotalMs). Одного счётчика мало — цикл из двадцати
// субмиллисекундных обращений в Redis стоит запросу 4мс, и алерт о нём — это
// разбуженный человек и ноль возможной выгоды (см. DefaultDetectorConfig).
//
// Исключение из пола по времени — группа с НУЛЕВЫМ суммарным временем и
// количеством от zeroClockCountFactor порогов: нулевое время означает не
// «дёшево», а «часы спанов сломаны» (SDK с миллисекундной обрезкой честно
// отчитывается о 0 у субмиллисекундного запроса к Redis). Без исключения
// настоящий цикл из сорока обращений на таком SDK молчал бы совсем — а сорок
// обращений в БД из одного места это проблема независимо от того, что SDK
// сообщил об их длительности. Ложную тревогу это не создаёт: гейт по КОЛИЧЕСТВУ
// поднят вдвое, и группа поменьше (те же нулевые часы, шесть спанов) молчит.
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
		// Пол по времени не применяется к группе со сломанными часами (totalUS == 0),
		// если она вдвое многочисленнее порога: см. комментарий к детектору.
		zeroClock := g.totalUS == 0 && g.count >= zeroClockCountFactor*cfg.NPlusOneMin
		if g.totalUS < minTotalUS && !zeroClock {
			return
		}
		ev := baseEvidence(g)
		ev["parent_op"] = g.parentOp
		out = append(out, newFinding(KindNPlusOne, "N+1 запросов: "+g.desc, t.Name, g.desc, ev))
	})
	return out
}

// detectSlowDBQueries: db-спан медленный, если он дольше абсолютного порога ИЛИ
// (дольше пола И съел заметную долю транзакции). Пол обязателен: правило «доля»
// без него срабатывает на КАЖДОМ здоровом простом эндпойнте — там один запрос и
// есть почти вся работа (12мс `GET /user` с единственным 8мс SELECT — это 67%
// транзакции и совершенно нормальный эндпойнт). Так же устроено у Sentry: доля
// — ДОПОЛНИТЕЛЬНОЕ условие, а не альтернатива абсолютному порогу.
//
// Пол выводится из конфига (SlowDBMs/slowDBShareFloorDivisor = 100мс на
// дефолтных 500мс), а не задаётся вторым магическим числом: проект, который
// опустил SlowDBMs до 50мс, ждёт и более чувствительной доли.
//
// Один и тот же запрос, медленный несколько раз в одной транзакции, — одна
// проблема с count=N, а не N проблем.
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
		out = append(out, newFinding(KindSlowDBQuery, "Медленный запрос: "+g.desc, t.Name, g.desc, ev))
	})
	return out
}

// detectHTTPFlood: много исходящих HTTP-вызовов, выпущенных ДРУГ ЗА ДРУГОМ.
// Смысл детектора — кандидат на распараллеливание, поэтому одного счётчика мало:
// дашборд, честно веером сходивший в 12 сервисов параллельно, — это ХОРОШО
// написанный эндпойнт, и алерт на него — ложная тревога на образцовом коде.
//
// Гейтов два, и достаточно любого:
//
//   - isSequential — сумма длительностей ≈ закрытое ими стенное время (вызовы
//     шли строго друг за другом);
//   - водопад с ОГРАНИЧЕННЫМ параллелизмом: вызовов вдвое-втрое больше, чем
//     помещается в пул (20 запросов по 2 одновременно). Доля стенного времени у
//     него ~50%, гейт последовательности он не проходит — а распараллелить его
//     ровно так же можно и нужно. Признак — «глубина» очереди
//     count/max_concurrency: сколько ВОЛН вызовов пришлось бы ждать. Волн от
//     HTTPFloodMin — это та же лавина, только через узкое горло.
//
// Здоровый веер при этом остаётся молчаливым по ОБОИМ гейтам: у 12 полностью
// параллельных вызовов max_concurrency = 12, глубина очереди = 1.
//
// Альтернативой гейтов могла бы быть «доля стенного времени вызовов от
// транзакции», но она ровно этот веер и пропускает: 12 параллельных вызовов по
// 900мс в транзакции на 1000мс закрывают 90% её времени. Цена нынешнего решения:
// транзакция со сломанными часами SDK (нулевые длительности спанов) лавину не
// покажет — ни стенного времени, ни параллелизма по таким спанам не измерить, а
// молчание лучше ложной тревоги.
//
// Проблема принадлежит эндпойнту, а не конкретному URL (адреса в лавине
// разные), поэтому описание пустое, а фингерпринт держится на (kind, culprit).
// Сами адреса (нормализованные, до maxEvidenceURLs) кладутся в evidence: без них
// со страницы проблемы («Лавина HTTP-вызовов: GET /orders, count=12») не понять,
// куда именно ходил код. На фингерпринт они не влияют — он их не видит.
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
	// Измеренная доля стенного времени, а не флаг «sequential»: флаг после гейта
	// всегда true и не сообщает ничего, а доля показывает, НАСКОЛЬКО вызовы
	// последовательны (100% — цепочка, ~50% — водопад по два в полёте).
	ev["sequential_pct"] = sequentialPct(g.totalUS, wallUS)
	ev["max_concurrency"] = concurrency
	// Это op САМОЙ транзакции (http.server), а не родителя вызовов: у лавины
	// родителя нет — вызовы принадлежат эндпойнту целиком.
	ev["transaction_op"] = t.Op
	ev["urls"] = urls

	title := fmt.Sprintf("Лавина HTTP-вызовов: %s", t.Name)
	return []Finding{newFinding(KindHTTPFlood, title, t.Name, "", ev)}
}

// isSequential: вызовы шли друг за другом, если их суммарная длительность почти
// равна стенному времени, которое они закрыли. Параллельный веер закрывает
// стенного времени МЕНЬШЕ суммы (12 одновременных вызовов по 900мс — это 10800мс
// суммы на 900мс стены), и последовательным не считается.
//
// Сравнивать сумму с длительностью ТРАНЗАКЦИИ нельзя: тот же веер даёт 1080% от
// неё и уверенно проходит любой порог — флаг, придуманный помечать кандидатов на
// распараллеливание, помечал бы ровно уже распараллеленное.
func isSequential(totalUS, wallUS uint64) bool {
	if totalUS == 0 || wallUS == 0 {
		return false
	}
	return wallUS*100 >= totalUS*httpSequentialPercent
}

// sequentialPct — та же величина, что сравнивает isSequential, но числом: доля
// стенного времени, закрытого вызовами, от суммы их длительностей. 100% —
// строгая цепочка, ~50% — по два вызова в полёте, ~8% — веер из двенадцати.
func sequentialPct(totalUS, wallUS uint64) int {
	if totalUS == 0 {
		return 0
	}
	return int(wallUS * 100 / totalUS)
}

// isBoundedWaterfall — водопад с ограниченным параллелизмом: вызовов в
// count/concurrency раз больше, чем их влезает в полёт одновременно. Это ЧИСЛО
// ВОЛН, которые эндпойнт отстоял в очереди; от порога лавины и выше — такой же
// кандидат на распараллеливание, как цепочка (см. detectHTTPFlood). Полностью
// параллельный веер даёт глубину 1 и молчит.
func isBoundedWaterfall(count, concurrency, floodMin int) bool {
	if concurrency <= 0 {
		return false // часы SDK сломаны: параллелизм не измерить
	}
	return count/concurrency >= floodMin
}

// maxConcurrency — сколько спанов максимум было в полёте одновременно. Считается
// разметкой границ интервалов: +1 на начале, -1 на конце, максимум бегущей
// суммы. Спаны с нулевой длительностью (сломанные часы SDK) не участвуют — так
// же, как в mergedWallUS.
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

// mergedWallUS — стенное время, покрытое спанами: интервалы [Start, End)
// сливаются, пересечения не считаются дважды. Чистая функция: сортируется копия.
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

// baseEvidence — общие поля evidence. Время в МИКРОсекундах: Redis-спаны бывают
// субмиллисекундными, и в миллисекундах N+1 из восьми таких запросов честно
// показал бы total=0.
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

// newFinding. В СТРОКЕ проблемы (Culprit, Title) остаётся сырое имя транзакции —
// его читает человек. В ФИНГЕРПРИНТ идёт нормализованное (см.
// fingerprintCulprit): иначе приложение, не шаблонизирующее маршруты, плодит по
// проблеме на КАЖДЫЙ запрос.
func newFinding(kind, title, culprit, desc string, ev map[string]any) Finding {
	return Finding{
		Kind:        kind,
		Title:       capRunes(title, maxNormalizedDescription+64),
		Culprit:     culprit,
		Fingerprint: fingerprintOf(kind, fingerprintCulprit(culprit), desc),
		Description: desc,
		Evidence:    ev,
	}
}

// fingerprintCulprit — имя транзакции, приведённое к шаблону маршрута:
// `GET /users/42` → `GET /users/{id}`. Без этого приложение, которое не
// шаблонизирует имена (или OTLP-спан без http.route), даёт НОВОЕ имя на каждый
// запрос — а значит новый фингерпринт, новую строку perf_issues и новый алерт на
// каждый запрос: при 50 rps это ~180 тысяч строк и столько же сообщений в час.
// Нормализация схлопывает их в одну проблему.
//
// Пустое имя (пробелы, отсутствующее имя OTLP-спана) остаётся пустым: Detect
// такие транзакции пропускает целиком, а не склеивает в общую проблему.
func fingerprintCulprit(name string) string {
	return strings.TrimSpace(NormalizeURL(name))
}

// fingerprintOf — стабильный идентификатор проблемы: одинаковый на любой
// реплике и после рестарта (чистый sha256, никаких карт и указателей).
// project_id сюда НЕ входит: он часть уникального ключа perf_issues и
// добавляется upsert-ом.
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

// hasOpPrefix — сопоставление op по префиксу: `db` матчит и `db.sql.query`, и
// `db.redis`, но не `dbutil.something`.
func hasOpPrefix(op, prefix string) bool {
	op = strings.ToLower(strings.TrimSpace(op))
	if !strings.HasPrefix(op, prefix) {
		return false
	}
	return len(op) == len(prefix) || op[len(prefix)] == '.'
}

// spanOps — op спанов по их id, включая корневой спан транзакции: родителем
// db-спана обычно оказывается либо запрос-«шапка», либо сама транзакция.
func spanOps(t Transaction) map[string]string {
	ops := make(map[string]string, len(t.Spans)+1)
	ops[t.SpanID] = t.Op
	for _, s := range t.Spans {
		ops[s.SpanID] = s.Op
	}
	return ops
}
