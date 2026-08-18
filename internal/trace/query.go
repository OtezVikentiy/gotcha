package trace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query — чтение агрегатов производительности из ClickHouse. По образцу
// internal/uptime/query.go и internal/event/query.go: параметризованные
// запросы (значения только через ?, никогда не конкатенируются в текст),
// выравнивание корзин по Unix epoch как в event.Query.Series.
//
// Список эндпойнтов (Endpoints, EndpointLatency при кратном 5м шаге) читается
// из материализованной вьюхи transactions_5m (AggregatingMergeTree) через
// -Merge-комбинаторы — это на порядки дешевле, чем агрегировать сырые
// transactions. Waterfall и примеры трейсов (SlowestTraces, Trace) читают
// сырые spans/transactions по trace_id (префиксу ключа сортировки).
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

// EndpointStat — строка списка эндпойнтов: агрегаты за период.
type EndpointStat struct {
	Transaction string
	Count       uint64  // число транзакций за период
	Throughput  float64 // транзакций в минуту
	P50         uint32  // микросекунды
	P75         uint32
	P95         uint32
	P99         uint32
	FailureRate float64 // доля транзакций со status != 'ok', 0..1
	ApdexScore  float64 // (satisfied + tolerating/2) / total, 0..1
	// Environments — окружения, в которых эндпойнт встречался за период.
	// Список, а не одно значение: строка агрегирует транзакции по всем
	// окружениям сразу, и разбивать её по окружениям означало бы менять
	// смысл строки (один эндпойнт показывался бы дважды с разными
	// перцентилями).
	Environments []string
}

// LatencyPoint — точка временного ряда латентности эндпойнта: T — начало
// интервала (UTC), P50/P95 — перцентили в микросекундах, Count — число
// транзакций в интервале (0, если их не было).
type LatencyPoint struct {
	T     time.Time
	P50   uint32
	P95   uint32
	Count uint64
}

// DurationBucket — корзина гистограммы длительностей: UpperUS — верхняя
// граница корзины (микросекунды), Count — число транзакций в корзине.
type DurationBucket struct {
	UpperUS uint32
	Count   uint64
}

// TraceRow — трейс как пример (для «самые медленные») или корень waterfall.
type TraceRow struct {
	TraceID    string
	DurationUS uint32
	Timestamp  time.Time
	Status     string
}

// SpanRow — спан для waterfall: StartUS — смещение начала спана от начала
// трейса (микросекунды), DurationUS — длительность.
type SpanRow struct {
	SpanID       string
	ParentSpanID string
	Op           string
	Description  string
	Status       string
	StartUS      uint32
	DurationUS   uint32
}

// Endpoints возвращает список эндпойнтов проекта за [from, to), отсортированный
// по числу транзакций по убыванию. Count/перцентили/failure rate читаются из
// MV transactions_5m (-Merge-комбинаторы). Apdex считается отдельным запросом
// из сырых transactions: MV хранит только квантили, а не пороговые счётчики
// длительностей, поэтому вычислить satisfied/tolerating из неё нельзя. environment
// пустой → без фильтра по окружению; apdexT — порог в миллисекундах (≤ 0 →
// Apdex не считается и остаётся 0).
func (q *Query) Endpoints(ctx context.Context, projectID int64, from, to time.Time, environment string, apdexT int) ([]EndpointStat, error) {
	where := "project_id = ? AND bucket >= ? AND bucket < ?"
	args := []any{uint64(projectID), from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT transaction,
			countMerge(cnt) AS c,
			countMerge(failures) AS f,
			quantilesMerge(0.5, 0.75, 0.95, 0.99)(dur) AS q,
			arraySort(groupUniqArray(environment)) AS envs
		FROM transactions_5m
		WHERE `+where+`
		GROUP BY transaction
		ORDER BY c DESC, transaction`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: endpoints: %w", err)
	}
	defer rows.Close()

	periodMin := to.Sub(from).Minutes()
	var out []EndpointStat
	for rows.Next() {
		var s EndpointStat
		var failures uint64
		var qs []float64
		if err := rows.Scan(&s.Transaction, &s.Count, &failures, &qs, &s.Environments); err != nil {
			return nil, fmt.Errorf("trace: endpoints: scan: %w", err)
		}
		if len(qs) == 4 {
			s.P50 = usFromFloat(qs[0])
			s.P75 = usFromFloat(qs[1])
			s.P95 = usFromFloat(qs[2])
			s.P99 = usFromFloat(qs[3])
		}
		if s.Count > 0 {
			s.FailureRate = float64(failures) / float64(s.Count)
		}
		if periodMin > 0 {
			s.Throughput = float64(s.Count) / periodMin
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: endpoints: %w", err)
	}

	if apdexT > 0 {
		apdex, err := q.apdexByTransaction(ctx, projectID, from, to, environment, apdexT)
		if err != nil {
			return nil, err
		}
		for i := range out {
			out[i].ApdexScore = apdex[out[i].Transaction]
		}
	}
	return out, nil
}

// Dependency — одна внешняя зависимость сервиса (узел карты C4): вид (database/
// cache/http), цель (db.system / хранилище / хост) и агрегаты вызовов за окно.
type Dependency struct {
	Kind      string // database | cache | http
	Target    string // postgresql | redis | api.stripe.com | ...
	Calls     int64
	P50US     uint32
	P95US     uint32
	ErrorRate float64 // доля спанов со status != 'ok'
}

// Dependencies агрегирует внешние зависимости сервиса из client-op спанов за
// окно [from,to): узлы карты C4. kind/target выводятся в SQL из op/data/
// description (подзапрос, чтобы GROUP BY шёл по уже вычисленным полям). http.server
// (сама транзакция) и internal-op в фильтр НЕ входят. Считается по СЫРЫМ spans
// (не по MV) — окно защищено max_execution_time + LIMIT + дефолт-окном на слое web.
func (q *Query) Dependencies(ctx context.Context, projectID int64, from, to time.Time, limit int) ([]Dependency, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.conn.Query(ctx, `
		SELECT kind, target, count() AS c, countIf(status != 'ok') AS f,
			quantiles(0.5, 0.95)(duration_us) AS q
		FROM (
			SELECT
				multiIf(op = 'db' OR startsWith(op,'db.sql') OR op = 'db.query', 'database',
						startsWith(op,'db.'), 'cache',
						'http') AS kind,
				multiIf(
					op = 'db' OR startsWith(op,'db.sql') OR op = 'db.query',
						coalesce(nullIf(JSONExtractString(data,'db.system'),''), nullIf(JSONExtractString(data,'db.system.name'),''), 'database'),
					startsWith(op,'db.'),
						arrayElement(splitByChar('.', op), 2),
					coalesce(
						nullIf(JSONExtractString(data,'server.address'),''),
						nullIf(domain(JSONExtractString(data,'url.full')),''),
						nullIf(domain(replaceRegexpOne(description, '^\\S+\\s+', '')),''),
						'http')
				) AS target,
				status, duration_us
			FROM spans
			WHERE project_id = ? AND timestamp >= ? AND timestamp < ?
				AND (op = 'db' OR startsWith(op,'db.') OR op = 'http.client' OR startsWith(op,'http.client.'))
		)
		GROUP BY kind, target
		ORDER BY c DESC, target
		LIMIT ?
		SETTINGS max_execution_time = 10`,
		uint64(projectID), from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("trace: dependencies: %w", err)
	}
	defer rows.Close()

	var out []Dependency
	for rows.Next() {
		var d Dependency
		var calls, failures uint64
		var qs []float64
		if err := rows.Scan(&d.Kind, &d.Target, &calls, &failures, &qs); err != nil {
			return nil, fmt.Errorf("trace: dependencies: scan: %w", err)
		}
		d.Calls = int64(calls)
		if len(qs) == 2 {
			d.P50US = usFromFloat(qs[0])
			d.P95US = usFromFloat(qs[1])
		}
		if d.Calls > 0 {
			d.ErrorRate = float64(failures) / float64(d.Calls)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: dependencies: %w", err)
	}
	return out, nil
}

// apdexBoundsUS переводит apdex_threshold_ms (T) в границы µs для
// countIf-запроса: satUS = T·1000, tolUS = 4·T·1000.
//
// duration_us в ClickHouse — UInt32 (макс ~4295с), а валидация настройки
// (projsettings.go) требует лишь apdex_threshold_ms > 0, без верхней границы.
// Без клампа satUS/tolUS в uint32 арифметике при apdexT выше ~4.29M мс молча
// переполняются и дают мусорный Apdex для всего проекта вместо клампа/ошибки
// (P2-6 из аудита 2026-08-12). Клампим до math.MaxUint32 — порога, который в
// любом случае удовлетворяет ЛЮБОЙ duration_us из колонки, так что клампленное
// значение не меняет смысл сравнения, только страхует от переполнения.
func apdexBoundsUS(apdexT int) (satUS, tolUS uint32) {
	satUS64 := uint64(apdexT) * 1000
	if satUS64 > math.MaxUint32 {
		satUS64 = math.MaxUint32
	}
	tolUS64 := satUS64 * 4
	if tolUS64 > math.MaxUint32 {
		tolUS64 = math.MaxUint32
	}
	return uint32(satUS64), uint32(tolUS64)
}

// apdexByTransaction считает Apdex каждого эндпойнта из сырых transactions.
// satisfied = duration ≤ T·1000 µs, tolerating = T·1000 < duration ≤ 4·T·1000;
// Apdex = (satisfied + tolerating/2) / total = (satisfied + within4T) / (2·total).
func (q *Query) apdexByTransaction(ctx context.Context, projectID int64, from, to time.Time, environment string, apdexT int) (map[string]float64, error) {
	satUS, tolUS := apdexBoundsUS(apdexT)

	where := "project_id = ? AND timestamp >= ? AND timestamp < ?"
	args := []any{satUS, tolUS, uint64(projectID), from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT transaction,
			countIf(duration_us <= ?) AS satisfied,
			countIf(duration_us <= ?) AS within4t,
			count() AS total
		FROM transactions
		WHERE `+where+`
		GROUP BY transaction`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: apdex: %w", err)
	}
	defer rows.Close()

	out := make(map[string]float64)
	for rows.Next() {
		var transaction string
		var satisfied, within4t, total uint64
		if err := rows.Scan(&transaction, &satisfied, &within4t, &total); err != nil {
			return nil, fmt.Errorf("trace: apdex: scan: %w", err)
		}
		if total > 0 {
			out[transaction] = float64(satisfied+within4t) / (2 * float64(total))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: apdex: %w", err)
	}
	return out, nil
}

// EndpointLatency строит временной ряд p50/p95/count эндпойнта на окне
// [from, to) с шагом step: точки идут по шагу от from до to включительно,
// пропуски заполняются нулями. Сетка выровнена по Unix epoch (как
// event.Query.Series). Если step кратен 5 минутам, ряд собирается из MV
// transactions_5m (дёшево); иначе — из сырых transactions.
func (q *Query) EndpointLatency(ctx context.Context, projectID int64, transaction string, from, to time.Time, step time.Duration, environment string) ([]LatencyPoint, error) {
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("trace: endpoint latency: step must be at least one second, got %s", step)
	}

	fromMV := step >= 5*time.Minute && step%(5*time.Minute) == 0

	var (
		table   string
		timeCol string
	)
	if fromMV {
		table, timeCol = "transactions_5m", "bucket"
	} else {
		table, timeCol = "transactions", "timestamp"
	}

	where := "project_id = ? AND transaction = ? AND " + timeCol + " >= ? AND " + timeCol + " < ?"
	args := []any{stepSec, uint64(projectID), transaction, from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	var selectExpr string
	if fromMV {
		selectExpr = `countMerge(cnt) AS c, quantilesMerge(0.5, 0.75, 0.95, 0.99)(dur) AS q`
	} else {
		selectExpr = `count() AS c, quantiles(0.5, 0.95)(duration_us) AS q`
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(`+timeCol+`, INTERVAL ? second) AS bucket_ts, `+selectExpr+`
		FROM `+table+`
		WHERE `+where+`
		GROUP BY bucket_ts
		ORDER BY bucket_ts`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: endpoint latency: %w", err)
	}
	defer rows.Close()

	byBucket := make(map[int64]LatencyPoint)
	for rows.Next() {
		var t time.Time
		var c uint64
		var qs []float64
		if err := rows.Scan(&t, &c, &qs); err != nil {
			return nil, fmt.Errorf("trace: endpoint latency: scan: %w", err)
		}
		p := LatencyPoint{Count: c}
		// MV: q = [p50,p75,p95,p99] (индексы 0 и 2); raw: q = [p50,p95].
		if fromMV && len(qs) == 4 {
			p.P50 = usFromFloat(qs[0])
			p.P95 = usFromFloat(qs[2])
		} else if !fromMV && len(qs) == 2 {
			p.P50 = usFromFloat(qs[0])
			p.P95 = usFromFloat(qs[1])
		}
		byBucket[t.UTC().Unix()] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: endpoint latency: %w", err)
	}

	// Align grid to Unix epoch like ClickHouse toStartOfInterval does.
	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	// Последняя корзина — та, что СОДЕРЖИТ момент to, а не следующая за ним.
	// Раньше граница округлялась ВВЕРХ за to, а цикл шёл по `<=`, поэтому в ряд
	// добавлялась корзина, начинающаяся в to или позже: запрос фильтрует ts < to,
	// так что данных в ней не могло быть ни при каких условиях. Спарклайны в
	// списках (мониторы, эндпойнты) не проходят через fillSeries и потому
	// заканчивались принудительным падением в ноль.
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}

	var out []LatencyPoint
	for curUnix := startUnix; curUnix <= endUnix; curUnix += stepSec {
		p := byBucket[curUnix] // zero LatencyPoint if bucket has no transactions
		p.T = time.Unix(curUnix, 0).UTC()
		out = append(out, p)
	}
	return out, nil
}

// EndpointLatencyBatch — EndpointLatency сразу для нескольких transactions ОДНИМ
// запросом к ClickHouse (WHERE transaction IN ? вместо N отдельных запросов).
// Список эндпойнтов (performanceList) собирает спарклайн на каждую строку —
// раньше это было до perfEndpointLimit последовательных round-trip'ов на
// загрузку страницы; здесь один. Сетка точек, заполнение пропусков и выбор
// MV/сырых transactions — 1:1 с EndpointLatency (та же логика, применена per-
// transaction после разбора одного набора строк). Пустой transactions → пустая
// карта без обращения к ClickHouse. Отсутствующий в БД transaction получает
// такой же ряд из нулевых точек, что и одиночный EndpointLatency для него.
func (q *Query) EndpointLatencyBatch(ctx context.Context, projectID int64, transactions []string, from, to time.Time, step time.Duration, environment string) (map[string][]LatencyPoint, error) {
	out := make(map[string][]LatencyPoint, len(transactions))
	if len(transactions) == 0 {
		return out, nil
	}

	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("trace: endpoint latency batch: step must be at least one second, got %s", step)
	}

	fromMV := step >= 5*time.Minute && step%(5*time.Minute) == 0

	var (
		table   string
		timeCol string
	)
	if fromMV {
		table, timeCol = "transactions_5m", "bucket"
	} else {
		table, timeCol = "transactions", "timestamp"
	}

	where := "project_id = ? AND transaction IN ? AND " + timeCol + " >= ? AND " + timeCol + " < ?"
	args := []any{stepSec, uint64(projectID), transactions, from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	var selectExpr string
	if fromMV {
		selectExpr = `countMerge(cnt) AS c, quantilesMerge(0.5, 0.75, 0.95, 0.99)(dur) AS q`
	} else {
		selectExpr = `count() AS c, quantiles(0.5, 0.95)(duration_us) AS q`
	}

	rows, err := q.conn.Query(ctx, `
		SELECT transaction, toStartOfInterval(`+timeCol+`, INTERVAL ? second) AS bucket_ts, `+selectExpr+`
		FROM `+table+`
		WHERE `+where+`
		GROUP BY transaction, bucket_ts
		ORDER BY transaction, bucket_ts`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: endpoint latency batch: %w", err)
	}
	defer rows.Close()

	byTxBucket := make(map[string]map[int64]LatencyPoint, len(transactions))
	for rows.Next() {
		var tx string
		var t time.Time
		var c uint64
		var qs []float64
		if err := rows.Scan(&tx, &t, &c, &qs); err != nil {
			return nil, fmt.Errorf("trace: endpoint latency batch: scan: %w", err)
		}
		p := LatencyPoint{Count: c}
		// MV: q = [p50,p75,p95,p99] (индексы 0 и 2); raw: q = [p50,p95].
		if fromMV && len(qs) == 4 {
			p.P50 = usFromFloat(qs[0])
			p.P95 = usFromFloat(qs[2])
		} else if !fromMV && len(qs) == 2 {
			p.P50 = usFromFloat(qs[0])
			p.P95 = usFromFloat(qs[1])
		}
		byBucket, ok := byTxBucket[tx]
		if !ok {
			byBucket = make(map[int64]LatencyPoint)
			byTxBucket[tx] = byBucket
		}
		byBucket[t.UTC().Unix()] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: endpoint latency batch: %w", err)
	}

	// Та же выравненная по epoch сетка, что в EndpointLatency, построенная один
	// раз и применённая к каждому transaction — раскладка по map, а не N
	// запросов, и есть весь смысл батча.
	fromUnix := from.UTC().Unix()
	toUnix := to.UTC().Unix()
	startUnix := (fromUnix / stepSec) * stepSec
	endUnix := ((toUnix - 1) / stepSec) * stepSec
	if endUnix < startUnix {
		endUnix = startUnix
	}

	for _, tx := range transactions {
		byBucket := byTxBucket[tx] // nil, если transaction не встретился ни разу — ниже даст ряд нулей
		series := make([]LatencyPoint, 0, (endUnix-startUnix)/stepSec+1)
		for curUnix := startUnix; curUnix <= endUnix; curUnix += stepSec {
			p := byBucket[curUnix]
			p.T = time.Unix(curUnix, 0).UTC()
			series = append(series, p)
		}
		out[tx] = series
	}
	return out, nil
}

// DurationHistogram строит гистограмму длительностей эндпойнта за [from, to)
// из сырых transactions: buckets корзин равной ширины по длительности. Ширина
// определяется по максимальной длительности за период. UpperUS корзины i —
// (i+1)·width. Сумма Count по корзинам равна числу транзакций. Пусто → nil.
func (q *Query) DurationHistogram(ctx context.Context, projectID int64, transaction string, from, to time.Time, environment string, buckets int) ([]DurationBucket, error) {
	if buckets <= 0 {
		return nil, nil
	}

	where := "project_id = ? AND transaction = ? AND timestamp >= ? AND timestamp < ?"
	baseArgs := []any{uint64(projectID), transaction, from, to}
	if environment != "" {
		where += " AND environment = ?"
		baseArgs = append(baseArgs, environment)
	}

	var maxDur uint32
	var total uint64
	if err := q.conn.QueryRow(ctx,
		`SELECT max(duration_us), count() FROM transactions WHERE `+where, baseArgs...).
		Scan(&maxDur, &total); err != nil {
		return nil, fmt.Errorf("trace: duration histogram: max: %w", err)
	}
	if total == 0 {
		return nil, nil
	}

	width := maxDur / uint32(buckets)
	if width == 0 {
		width = 1
	}

	// Границу корзины кладём через least(intDiv(duration, width), buckets-1):
	// значения == maxDur не должны выпасть за пределы последней корзины.
	args := append([]any{width, uint32(buckets - 1)}, baseArgs...)
	rows, err := q.conn.Query(ctx, `
		SELECT least(intDiv(duration_us, ?), ?) AS b, count() AS n
		FROM transactions
		WHERE `+where+`
		GROUP BY b`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: duration histogram: %w", err)
	}
	defer rows.Close()

	out := make([]DurationBucket, buckets)
	for i := range out {
		out[i].UpperUS = uint32(i+1) * width
	}
	for rows.Next() {
		var b uint32
		var n uint64
		if err := rows.Scan(&b, &n); err != nil {
			return nil, fmt.Errorf("trace: duration histogram: scan: %w", err)
		}
		idx := int(b)
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx].Count += n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: duration histogram: %w", err)
	}
	return out, nil
}

// SlowestTraces возвращает до n самых медленных трейсов эндпойнта за [from, to)
// из сырых transactions, отсортированных по длительности по убыванию.
func (q *Query) SlowestTraces(ctx context.Context, projectID int64, transaction string, from, to time.Time, n int) ([]TraceRow, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT trace_id, duration_us, timestamp, status
		FROM transactions
		WHERE project_id = ? AND transaction = ? AND timestamp >= ? AND timestamp < ?
		ORDER BY duration_us DESC
		LIMIT ?`,
		uint64(projectID), transaction, from, to, n)
	if err != nil {
		return nil, fmt.Errorf("trace: slowest traces: %w", err)
	}
	defer rows.Close()

	out := make([]TraceRow, 0, n)
	for rows.Next() {
		var r TraceRow
		if err := rows.Scan(&r.TraceID, &r.DurationUS, &r.Timestamp, &r.Status); err != nil {
			return nil, fmt.Errorf("trace: slowest traces: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: slowest traces: %w", err)
	}
	return out, nil
}

// SpanRetentionDays — TTL таблицы spans по умолчанию при первой установке
// (internal/db/migrations/ch/0004_spans.up.sql: TTL toDateTime(timestamp) +
// INTERVAL 30 DAY). transactions живёт дольше (90 дней при дефолте,
// ch/0003_transactions.up.sql), поэтому трейс подходящего возраста ещё виден
// в списках (Endpoints/SlowestTraces читают transactions), но Trace() для
// него уже не находит ни одного спана — waterfall рисовать нечем.
//
// ВАЖНО: TTL spans настраивается в проде (GOTCHA_SPAN_RETENTION_DAYS,
// применяется на каждом старте через db.ApplySpanRetention →
// ALTER TABLE spans MODIFY TTL) и на не-дефолтных инстансах отличается от
// этой константы. web.Handler.SpanRetentionDays (проставляется из
// cfg.SpanRetentionDays в main.go) — источник истины для UI
// (web.traceWaterfall, templates.EndpointDetail.Slowest); эта константа —
// только дефолт первой установки, годится для доков и тестов, но НЕ для
// сравнения с реальным возрастом трейса в хендлерах.
const SpanRetentionDays = 30

// traceSpanLimit — верхняя граница числа спанов, читаемых Trace из ClickHouse.
// Патологический трейс (десятки тысяч спанов) не должен грузиться в память
// целиком; лимит с запасом превышает потолок waterfall (waterfallMaxRows=200 в
// internal/web), который дополнительно усекает отрисовку.
const traceSpanLimit = 5000

// Trace возвращает спаны трейса для waterfall из сырых spans, отсортированные по
// timestamp (при равенстве — по span_id для стабильности), не более
// traceSpanLimit штук. StartUS каждого спана — смещение от начала трейса
// (минимального timestamp). root — корневой спан (без родителя; если такого нет
// — первый по времени). Пустой результат (spans == nil) означает, что трейс не
// найден.
func (q *Query) Trace(ctx context.Context, projectID int64, traceID string) (root TraceRow, spans []SpanRow, err error) {
	rows, err := q.conn.Query(ctx, `
		SELECT span_id, parent_span_id, op, description, status, timestamp, duration_us
		FROM spans
		WHERE project_id = ? AND trace_id = ?
		ORDER BY timestamp, span_id
		LIMIT ?`,
		uint64(projectID), traceID, traceSpanLimit)
	if err != nil {
		return TraceRow{}, nil, fmt.Errorf("trace: trace: %w", err)
	}
	defer rows.Close()

	var stamps []time.Time
	for rows.Next() {
		var s SpanRow
		var ts time.Time
		if err := rows.Scan(&s.SpanID, &s.ParentSpanID, &s.Op, &s.Description, &s.Status, &ts, &s.DurationUS); err != nil {
			return TraceRow{}, nil, fmt.Errorf("trace: trace: scan: %w", err)
		}
		spans = append(spans, s)
		stamps = append(stamps, ts.UTC())
	}
	if err := rows.Err(); err != nil {
		return TraceRow{}, nil, fmt.Errorf("trace: trace: %w", err)
	}
	if len(spans) == 0 {
		return TraceRow{}, nil, nil
	}

	minTS := stamps[0] // ORDER BY timestamp → первый минимальный
	for i := range spans {
		off := stamps[i].Sub(minTS).Microseconds()
		if off < 0 {
			off = 0
		}
		if off > math.MaxUint32 {
			off = math.MaxUint32
		}
		spans[i].StartUS = uint32(off)
	}

	root = TraceRow{TraceID: traceID, Timestamp: minTS}
	rootFound := false
	for i := range spans {
		if spans[i].ParentSpanID == "" {
			root.DurationUS = spans[i].DurationUS
			root.Status = spans[i].Status
			rootFound = true
			break
		}
	}
	if !rootFound {
		root.DurationUS = spans[0].DurationUS
		root.Status = spans[0].Status
	}
	return root, spans, nil
}

// maxOffendingSpans — потолок загружаемых спанов проблемы (evidence.span_ids
// уже ≤10, но страхуемся от кривых данных).
const maxOffendingSpans = 10

// SpanDetail — спан с ПОЛНЫМ описанием и распарсенным data. Нужен странице
// perf-проблемы: полный текст запроса (в отличие от нормализованного и
// обрезанного Title проблемы) и опциональная привязка к коду из data
// (code.filepath/code.lineno/code.function, db.system — если их прислал SDK).
type SpanDetail struct {
	SpanID      string
	Op          string
	Description string
	DurationUS  uint32
	Data        map[string]string
}

// OffendingSpans загружает конкретные спаны трейса по их id (обычно из
// evidence.span_ids проблемы) с полным description и data, отсортированные по
// длительности убыв. — первым идёт самый показательный. Пустой traceID/список,
// истёкший по TTL трейс или отсутствие спанов дают nil без ошибки: страница
// проблемы должна отрисоваться и без примера.
func (q *Query) OffendingSpans(ctx context.Context, projectID int64, traceID string, spanIDs []string) ([]SpanDetail, error) {
	if traceID == "" || len(spanIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(spanIDs))
	for _, s := range spanIDs {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			ids = append(ids, s)
		}
		if len(ids) >= maxOffendingSpans {
			break
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT span_id, op, description, duration_us, data
		FROM spans
		WHERE project_id = ? AND trace_id = ? AND span_id IN ?
		ORDER BY duration_us DESC
		LIMIT ?`,
		uint64(projectID), traceID, ids, maxOffendingSpans)
	if err != nil {
		return nil, fmt.Errorf("trace: offending spans: %w", err)
	}
	defer rows.Close()

	var out []SpanDetail
	for rows.Next() {
		var s SpanDetail
		var data string
		if err := rows.Scan(&s.SpanID, &s.Op, &s.Description, &s.DurationUS, &data); err != nil {
			return nil, fmt.Errorf("trace: offending spans: scan: %w", err)
		}
		s.Data = decodeSpanData(data)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: offending spans: %w", err)
	}
	return out, nil
}

// decodeSpanData разбирает JSON-строку колонки data спана в плоскую
// map[string]string: строковые значения как есть, числовые/булевы — их текст;
// вложенные объекты/массивы пропускаются (нам нужны скалярные ключи вроде
// code.filepath/code.lineno). Пустой/битый JSON → nil.
func decodeSpanData(s string) map[string]string {
	if s == "" || s == "{}" || s == "null" {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var str string
		if json.Unmarshal(v, &str) == nil {
			out[k] = str
			continue
		}
		t := strings.TrimSpace(string(v))
		if t == "" || t[0] == '{' || t[0] == '[' {
			continue
		}
		out[k] = t
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Environments возвращает окружения проекта, встречавшиеся у транзакций за
// [from, to), по алфавиту — для наполнения select фильтра на странице
// производительности. Читается из MV transactions_5m (дёшево, как Endpoints).
func (q *Query) Environments(ctx context.Context, projectID int64, from, to time.Time) ([]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT environment
		FROM transactions_5m
		WHERE project_id = ? AND bucket >= ? AND bucket < ?
		ORDER BY environment`,
		uint64(projectID), from, to)
	if err != nil {
		return nil, fmt.Errorf("trace: environments: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var env string
		if err := rows.Scan(&env); err != nil {
			return nil, fmt.Errorf("trace: environments: scan: %w", err)
		}
		if env == "" {
			continue
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: environments: %w", err)
	}
	return out, nil
}

// ProjectForTrace ищет проект, которому принадлежит трейс, по trace_id из
// сырых transactions (для связки ошибка→трейс, когда известен только
// trace_id). found=false, если трейса нет. При (пренебрежимо редкой) коллизии
// trace_id между проектами возвращает НЕДЕТЕРМИНИРОВАННЫЙ project_id (LIMIT 1
// без ORDER BY), поэтому вызывающий ОБЯЗАН проверить доступ к возвращённому id
// (обработчик /traces делает это через CanAccessProject).
func (q *Query) ProjectForTrace(ctx context.Context, traceID string) (projectID int64, found bool, err error) {
	row := q.conn.QueryRow(ctx, `
		SELECT project_id FROM transactions WHERE trace_id = ? LIMIT 1`, traceID)
	var pid uint64
	if err := row.Scan(&pid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("trace: project for trace: %w", err)
	}
	return int64(pid), true, nil
}

// TraceExistsInProject проверяет, есть ли у trace_id хоть одна транзакция в
// конкретном проекте. В отличие от ProjectForTrace (ищет по одному trace_id и
// потому обходит все партиции всех проектов), project_id здесь — префикс
// первичного ключа transactions (ORDER BY project_id, ...), поэтому запрос
// прунит гранулы до проекта. Используется на детали issue, где project_id уже
// известен, только чтобы решить, показывать ли ссылку «Смотреть трейс».
func (q *Query) TraceExistsInProject(ctx context.Context, projectID int64, traceID string) (bool, error) {
	row := q.conn.QueryRow(ctx, `
		SELECT 1 FROM transactions WHERE project_id = ? AND trace_id = ? LIMIT 1`,
		uint64(projectID), traceID)
	var one uint8
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("trace: exists in project: %w", err)
	}
	return true, nil
}

// Vital — один web vital с рейтингом: P75 — 75-й перцентиль (мс, кроме CLS),
// Count — число замеров за период. Rating — оценка Google по P75
// ("good"|"needs-improvement"|"poor"), либо "" если замеров нет (Count == 0).
type Vital struct {
	Name   string
	P75    float64
	Rating string
	Count  uint64
}

// PageVitals — сводка Web Vitals по странице (transaction): три ключевых
// показателя Core Web Vitals с рейтингом. Count — число замеров LCP (по нему
// же идёт сортировка списка страниц).
type PageVitals struct {
	Transaction string
	LCP         Vital
	INP         Vital
	CLS         Vital
	Count       uint64
	// Environments — окружения, в которых страница встречалась за период.
	// Список, а не одно значение: строка агрегирует замеры по всем
	// окружениям сразу (см. EndpointStat.Environments).
	Environments []string
}

// VitalPoint — точка временного ряда p75 одного vital: T — начало корзины
// (UTC), P75 — перцентиль в этой корзине. Пустые корзины (без замеров) в ряд
// не попадают.
type VitalPoint struct {
	T   time.Time
	P75 float64
}

// Пороги рейтинга Web Vitals (Google, по p75): значения в миллисекундах, кроме
// CLS (безразмерный). Граница good включительна (p75 == good → "good"), выше
// poor — "poor", между — "needs-improvement".
const (
	lcpGood, lcpPoor   = 2500.0, 4000.0
	inpGood, inpPoor   = 200.0, 500.0
	clsGood, clsPoor   = 0.1, 0.25
	fcpGood, fcpPoor   = 1800.0, 3000.0
	ttfbGood, ttfbPoor = 800.0, 1800.0
)

// webVitalsPageLimit — потолок числа страниц в списке WebVitalsPages.
const webVitalsPageLimit = 200

// Rating возвращает оценку Google для vital name по перцентилю p75
// ("good"|"needs-improvement"|"poor"). Для неизвестного имени — "".
// Отсутствие данных ("" при нуле замеров) обрабатывает вызывающий по Count, а
// не эта функция: p75 == 0 для известного vital (например CLS) — валидный
// "good".
func Rating(name string, p75 float64) string {
	var good, poor float64
	switch name {
	case "lcp":
		good, poor = lcpGood, lcpPoor
	case "inp":
		good, poor = inpGood, inpPoor
	case "cls":
		good, poor = clsGood, clsPoor
	case "fcp":
		good, poor = fcpGood, fcpPoor
	case "ttfb":
		good, poor = ttfbGood, ttfbPoor
	default:
		return ""
	}
	switch {
	case p75 <= good:
		return "good"
	case p75 > poor:
		return "poor"
	default:
		return "needs-improvement"
	}
}

// vitalKnown проверяет, что name — один из поддерживаемых web vitals. Нужен
// для VitalSeries: имя vital подставляется в текст запроса как имя колонки MV
// (не значение → не через ?), поэтому его допустимость проверяется белым
// списком до конкатенации.
func vitalKnown(name string) bool {
	switch name {
	case "lcp", "inp", "cls", "fcp", "ttfb":
		return true
	}
	return false
}

// makeVital собирает Vital из результатов quantilesMerge (одноэлементный
// массив p75) и countMerge (число замеров). При нуле замеров P75/Rating
// остаются нулевыми: quantilesMerge пустого состояния возвращает NaN, читать
// его нельзя, а рейтинг "" означает «нет данных».
func makeVital(name string, p75 []float64, count uint64) Vital {
	v := Vital{Name: name, Count: count}
	if count > 0 && len(p75) > 0 {
		v.P75 = p75[0]
		v.Rating = Rating(name, v.P75)
	}
	return v
}

// WebVitalsPages возвращает сводку Web Vitals по страницам проекта за [from, to)
// из MV web_vitals_5m: p75 LCP/INP/CLS (quantilesMerge(0.75)) с числом замеров
// (countMerge) и рейтингом. Отсортировано по числу замеров LCP по убыванию,
// не более webVitalsPageLimit страниц. environment пустой → без фильтра по
// окружению.
//
// HAVING отсекает транзакции без единого замера LCP/INP/CLS: MV агрегирует ВСЕ
// транзакции проекта (включая чистые API-эндпойнты без measurements), и без
// фильтра такие строки (Count 0, Rating "") засоряли бы хвост списка.
func (q *Query) WebVitalsPages(ctx context.Context, projectID int64, from, to time.Time, environment string) ([]PageVitals, error) {
	where := "project_id = ? AND bucket >= ? AND bucket < ?"
	args := []any{uint64(projectID), from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}
	args = append(args, webVitalsPageLimit)

	rows, err := q.conn.Query(ctx, `
		SELECT transaction,
			quantilesMerge(0.75)(lcp) AS lcp_p,
			quantilesMerge(0.75)(inp) AS inp_p,
			quantilesMerge(0.75)(cls) AS cls_p,
			countMerge(lcp_count) AS lcp_c,
			countMerge(inp_count) AS inp_c,
			countMerge(cls_count) AS cls_c,
			arraySort(groupUniqArray(environment)) AS envs
		FROM web_vitals_5m
		WHERE `+where+`
		GROUP BY transaction
		HAVING (lcp_c + inp_c + cls_c) > 0
		ORDER BY lcp_c DESC, transaction
		LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: web vitals pages: %w", err)
	}
	defer rows.Close()

	var out []PageVitals
	for rows.Next() {
		var transaction string
		var lcpP, inpP, clsP []float64
		var lcpC, inpC, clsC uint64
		var envs []string
		if err := rows.Scan(&transaction, &lcpP, &inpP, &clsP, &lcpC, &inpC, &clsC, &envs); err != nil {
			return nil, fmt.Errorf("trace: web vitals pages: scan: %w", err)
		}
		out = append(out, PageVitals{
			Transaction:  transaction,
			LCP:          makeVital("lcp", lcpP, lcpC),
			INP:          makeVital("inp", inpP, inpC),
			CLS:          makeVital("cls", clsP, clsC),
			Count:        lcpC,
			Environments: envs,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: web vitals pages: %w", err)
	}
	return out, nil
}

// VitalSeries строит временной ряд p75 одного vital (name: lcp|inp|cls|fcp|ttfb)
// страницы transaction на окне [from, to) с шагом step из MV web_vitals_5m
// (quantilesMerge(0.75)). В ряд попадают только корзины, где были замеры (пустые
// пропускаются — у VitalPoint нет счётчика, поэтому каждый P75 должен быть
// реальным). Сетка корзин выровнена по epoch средствами ClickHouse. environment
// пустой → без фильтра по окружению.
func (q *Query) VitalSeries(ctx context.Context, projectID int64, transaction, name string, from, to time.Time, step time.Duration, environment string) ([]VitalPoint, error) {
	if !vitalKnown(name) {
		return nil, fmt.Errorf("trace: vital series: unknown vital %q", name)
	}
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("trace: vital series: step must be at least one second, got %s", step)
	}

	where := "project_id = ? AND transaction = ? AND bucket >= ? AND bucket < ?"
	args := []any{stepSec, uint64(projectID), transaction, from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	// name белым списком проверен vitalKnown → безопасно подставить как имя
	// колонки (state — name, счётчик — name+"_count").
	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(bucket, INTERVAL ? second) AS bucket_ts,
			quantilesMerge(0.75)(`+name+`) AS q,
			countMerge(`+name+`_count) AS c
		FROM web_vitals_5m
		WHERE `+where+`
		GROUP BY bucket_ts
		HAVING c > 0
		ORDER BY bucket_ts`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: vital series: %w", err)
	}
	defer rows.Close()

	var out []VitalPoint
	for rows.Next() {
		var t time.Time
		var qs []float64
		var c uint64
		if err := rows.Scan(&t, &qs, &c); err != nil {
			return nil, fmt.Errorf("trace: vital series: scan: %w", err)
		}
		var p75 float64
		if len(qs) > 0 {
			p75 = qs[0]
		}
		out = append(out, VitalPoint{T: t.UTC(), P75: p75})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: vital series: %w", err)
	}
	return out, nil
}

// PageVitalsOne возвращает общий p75 всех пяти web vitals одной транзакции за
// [from, to) ОДНИМ запросом к MV web_vitals_5m: quantilesMerge(0.75) по каждому
// показателю и countMerge по числу замеров, агрегированные по всему окну без
// разбивки по корзинам (нет GROUP BY по времени → одна строка-агрегат).
// environment пустой → без фильтра по окружению. У vital без замеров Count == 0
// и Rating "" (см. makeVital) — по этому и решается, показывать ли панель.
func (q *Query) PageVitalsOne(ctx context.Context, projectID int64, transaction string, from, to time.Time, environment string) (lcp, inp, cls, fcp, ttfb Vital, err error) {
	where := "project_id = ? AND transaction = ? AND bucket >= ? AND bucket < ?"
	args := []any{uint64(projectID), transaction, from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	var lcpP, inpP, clsP, fcpP, ttfbP []float64
	var lcpC, inpC, clsC, fcpC, ttfbC uint64
	// Агрегат без GROUP BY всегда возвращает ровно одну строку (при отсутствии
	// замеров — с нулевыми countMerge и пустыми quantilesMerge), поэтому
	// ErrNoRows тут не бывает.
	if err := q.conn.QueryRow(ctx, `
		SELECT
			quantilesMerge(0.75)(lcp)  AS lcp_p,
			quantilesMerge(0.75)(inp)  AS inp_p,
			quantilesMerge(0.75)(cls)  AS cls_p,
			quantilesMerge(0.75)(fcp)  AS fcp_p,
			quantilesMerge(0.75)(ttfb) AS ttfb_p,
			countMerge(lcp_count)  AS lcp_c,
			countMerge(inp_count)  AS inp_c,
			countMerge(cls_count)  AS cls_c,
			countMerge(fcp_count)  AS fcp_c,
			countMerge(ttfb_count) AS ttfb_c
		FROM web_vitals_5m
		WHERE `+where, args...).
		Scan(&lcpP, &inpP, &clsP, &fcpP, &ttfbP, &lcpC, &inpC, &clsC, &fcpC, &ttfbC); err != nil {
		return Vital{}, Vital{}, Vital{}, Vital{}, Vital{}, fmt.Errorf("trace: page vitals one: %w", err)
	}
	return makeVital("lcp", lcpP, lcpC),
		makeVital("inp", inpP, inpC),
		makeVital("cls", clsP, clsC),
		makeVital("fcp", fcpP, fcpC),
		makeVital("ttfb", ttfbP, ttfbC), nil
}

// RecentVitalP75 читает p75 web-vital'а name (lcp|inp|cls|fcp|ttfb) страницы за
// окно [from, to) из MV web_vitals_5m и число замеров. Value уже в мс (для CLS —
// безразмерный), конвертация не нужна. name проверяется белым списком vitalKnown
// до подстановки как имя колонки (не bindable-параметр).
func (q *Query) RecentVitalP75(ctx context.Context, projectID int64, transaction, name string, from, to time.Time) (RegressionSample, error) {
	if !vitalKnown(name) {
		return RegressionSample{}, fmt.Errorf("trace: recent vital p75: unknown vital %q", name)
	}
	var p75 float64
	var cnt uint64
	if err := q.conn.QueryRow(ctx, `
		SELECT quantilesMerge(0.75)(`+name+`)[1] AS p75, countMerge(`+name+`_count) AS c
		FROM web_vitals_5m
		WHERE project_id = ? AND transaction = ? AND bucket >= ? AND bucket < ?`,
		uint64(projectID), transaction, from, to).Scan(&p75, &cnt); err != nil {
		return RegressionSample{}, fmt.Errorf("trace: recent vital p75: %w", err)
	}
	return valueSample(p75, cnt), nil
}

// BaselineVitalP75 — скользящая база web-vital'а name страницы: медиана дневных
// p75 за days дней (окно [now-days, now)), из MV web_vitals_5m. HAVING cnt > 0
// отбрасывает дни без замеров этого vital'а (иначе quantilesMerge пустого
// состояния вернул бы NaN и испортил медиану). Samples — сумма замеров за окно.
func (q *Query) BaselineVitalP75(ctx context.Context, projectID int64, transaction, name string, days int, now time.Time) (RegressionSample, error) {
	if !vitalKnown(name) {
		return RegressionSample{}, fmt.Errorf("trace: baseline vital p75: unknown vital %q", name)
	}
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	var med float64
	var total uint64
	if err := q.conn.QueryRow(ctx, `
		SELECT quantileExact(0.5)(daily) AS base, sum(cnt) AS total
		FROM (
			SELECT toStartOfDay(bucket) AS d,
				quantilesMerge(0.75)(`+name+`)[1] AS daily,
				countMerge(`+name+`_count) AS cnt
			FROM web_vitals_5m
			WHERE project_id = ? AND transaction = ? AND bucket >= ? AND bucket < ?
			GROUP BY d
			HAVING cnt > 0
		)`,
		uint64(projectID), transaction, from, now).Scan(&med, &total); err != nil {
		return RegressionSample{}, fmt.Errorf("trace: baseline vital p75: %w", err)
	}
	return valueSample(med, total), nil
}

// RecentEndpointP95s — свежие p95 сразу по списку эндпойнтов.
//
// Один запрос вместо запроса на эндпойнт. Прежний путь стоил оценщику по два
// обращения к ClickHouse на каждую цель — при топ-20 эндпойнтов и трёх
// web-vital'ах это больше двух сотен последовательных запросов на проект за
// тик, и обходились так все проекты подряд, независимо от трафика.
func (q *Query) RecentEndpointP95s(ctx context.Context, projectID int64, transactions []string, from, to time.Time) (map[string]RegressionSample, error) {
	out := make(map[string]RegressionSample, len(transactions))
	if len(transactions) == 0 {
		return out, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT transaction, quantilesMerge(0.95)(dur)[1] AS p95, countMerge(cnt) AS c
		FROM transactions_5m
		WHERE project_id = ? AND transaction IN ? AND bucket >= ? AND bucket < ?
		GROUP BY transaction`,
		uint64(projectID), transactions, from, to)
	if err != nil {
		return nil, fmt.Errorf("trace: recent endpoint p95s: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var p95 float64
		var cnt uint64
		if err := rows.Scan(&name, &p95, &cnt); err != nil {
			return nil, fmt.Errorf("trace: recent endpoint p95s scan: %w", err)
		}
		out[name] = msSample(p95, cnt)
	}
	return out, rows.Err()
}

// BaselineEndpointP95s — скользящие базы сразу по списку эндпойнтов: медиана
// дневных p95 за days дней.
func (q *Query) BaselineEndpointP95s(ctx context.Context, projectID int64, transactions []string, days int, now time.Time) (map[string]RegressionSample, error) {
	out := make(map[string]RegressionSample, len(transactions))
	if len(transactions) == 0 {
		return out, nil
	}
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := q.conn.Query(ctx, `
		SELECT transaction, quantileExact(0.5)(daily) AS base, sum(cnt) AS total
		FROM (
			SELECT transaction, toStartOfDay(bucket) AS d,
				quantilesMerge(0.95)(dur)[1] AS daily,
				countMerge(cnt) AS cnt
			FROM transactions_5m
			WHERE project_id = ? AND transaction IN ? AND bucket >= ? AND bucket < ?
			GROUP BY transaction, d
		)
		GROUP BY transaction`,
		uint64(projectID), transactions, from, now)
	if err != nil {
		return nil, fmt.Errorf("trace: baseline endpoint p95s: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var med float64
		var total uint64
		if err := rows.Scan(&name, &med, &total); err != nil {
			return nil, fmt.Errorf("trace: baseline endpoint p95s scan: %w", err)
		}
		out[name] = msSample(med, total)
	}
	return out, rows.Err()
}

// VitalKey — страница и метрика: ключ карт, возвращаемых vital-запросами.
type VitalKey struct {
	Transaction string
	Metric      string
}

// RecentVitalP75s — свежие p75 по списку страниц сразу по всем метрикам.
//
// Все метрики забираются одним запросом потому, что они лежат отдельными
// колонками одной таблицы: спрашивать их по одной значит перечитывать те же
// строки трижды.
func (q *Query) RecentVitalP75s(ctx context.Context, projectID int64, transactions, metrics []string, from, to time.Time) (map[VitalKey]RegressionSample, error) {
	out := make(map[VitalKey]RegressionSample, len(transactions)*len(metrics))
	if len(transactions) == 0 || len(metrics) == 0 {
		return out, nil
	}
	var parts []string
	for _, m := range metrics {
		if !vitalKnown(m) {
			return nil, fmt.Errorf("trace: recent vital p75s: unknown vital %q", m)
		}
		parts = append(parts,
			fmt.Sprintf("quantilesMerge(0.75)(%s)[1]", m),
			fmt.Sprintf("countMerge(%s_count)", m))
	}
	rows, err := q.conn.Query(ctx, `
		SELECT transaction, `+strings.Join(parts, ", ")+`
		FROM web_vitals_5m
		WHERE project_id = ? AND transaction IN ? AND bucket >= ? AND bucket < ?
		GROUP BY transaction`,
		uint64(projectID), transactions, from, to)
	if err != nil {
		return nil, fmt.Errorf("trace: recent vital p75s: %w", err)
	}
	defer rows.Close()
	if err := scanVitalRows(rows, metrics, out); err != nil {
		return nil, fmt.Errorf("trace: recent vital p75s: %w", err)
	}
	return out, rows.Err()
}

// BaselineVitalP75s — скользящие базы по списку страниц сразу по всем метрикам:
// медиана дневных p75 за days дней.
//
// Дни без замеров конкретной метрики поштучный запрос отбрасывал через HAVING;
// здесь строка дня общая для всех метрик, поэтому пустые дни отбрасываются
// условием внутри quantileExactIf — по счётчику этой же метрики.
func (q *Query) BaselineVitalP75s(ctx context.Context, projectID int64, transactions, metrics []string, days int, now time.Time) (map[VitalKey]RegressionSample, error) {
	out := make(map[VitalKey]RegressionSample, len(transactions)*len(metrics))
	if len(transactions) == 0 || len(metrics) == 0 {
		return out, nil
	}
	var inner, outer []string
	for _, m := range metrics {
		if !vitalKnown(m) {
			return nil, fmt.Errorf("trace: baseline vital p75s: unknown vital %q", m)
		}
		inner = append(inner,
			fmt.Sprintf("quantilesMerge(0.75)(%s)[1] AS %s_daily", m, m),
			fmt.Sprintf("countMerge(%s_count) AS %s_cnt", m, m))
		outer = append(outer,
			fmt.Sprintf("quantileExactIf(0.5)(%s_daily, %s_cnt > 0) AS %s_base", m, m, m),
			fmt.Sprintf("sum(%s_cnt) AS %s_total", m, m))
	}
	from := now.Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := q.conn.Query(ctx, `
		SELECT transaction, `+strings.Join(outer, ", ")+`
		FROM (
			SELECT transaction, toStartOfDay(bucket) AS d, `+strings.Join(inner, ", ")+`
			FROM web_vitals_5m
			WHERE project_id = ? AND transaction IN ? AND bucket >= ? AND bucket < ?
			GROUP BY transaction, d
		)
		GROUP BY transaction`,
		uint64(projectID), transactions, from, now)
	if err != nil {
		return nil, fmt.Errorf("trace: baseline vital p75s: %w", err)
	}
	defer rows.Close()
	if err := scanVitalRows(rows, metrics, out); err != nil {
		return nil, fmt.Errorf("trace: baseline vital p75s: %w", err)
	}
	return out, rows.Err()
}

// scanVitalRows раскладывает строку «страница + пары (значение, счётчик)» по
// ключам (страница, метрика).
func scanVitalRows(rows driver.Rows, metrics []string, out map[VitalKey]RegressionSample) error {
	for rows.Next() {
		var name string
		values := make([]float64, len(metrics))
		counts := make([]uint64, len(metrics))
		dest := make([]any, 0, 1+2*len(metrics))
		dest = append(dest, &name)
		for i := range metrics {
			dest = append(dest, &values[i], &counts[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		for i, m := range metrics {
			out[VitalKey{Transaction: name, Metric: m}] = valueSample(values[i], counts[i])
		}
	}
	return nil
}

// TopEndpointsByTraffic — топ-K имён эндпойнтов проекта по трафику за [from, to)
// (число транзакций, countMerge из transactions_5m), по убыванию. Для оценщика
// (план 4): регрессия на цели без нагрузки — шум, оцениваем только верхушку.
func (q *Query) TopEndpointsByTraffic(ctx context.Context, projectID int64, from, to time.Time, k int) ([]string, error) {
	if k <= 0 {
		return nil, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT transaction
		FROM transactions_5m
		WHERE project_id = ? AND bucket >= ? AND bucket < ?
		GROUP BY transaction
		ORDER BY countMerge(cnt) DESC, transaction
		LIMIT ?`,
		uint64(projectID), from, to, k)
	if err != nil {
		return nil, fmt.Errorf("trace: top endpoints by traffic: %w", err)
	}
	defer rows.Close()
	return scanStrings(rows, "trace: top endpoints by traffic")
}

// TopVitalPages — топ-K страниц с web-vital'ами за [from, to] по числу замеров
// (сумма countMerge по всем пяти vital'ам, из web_vitals_5m), по убыванию.
// HAVING отсекает транзакции без единого замера vital'а (MV агрегирует и чистые
// API-эндпойнты без measurements).
func (q *Query) TopVitalPages(ctx context.Context, projectID int64, from, to time.Time, k int) ([]string, error) {
	if k <= 0 {
		return nil, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT transaction
		FROM web_vitals_5m
		WHERE project_id = ? AND bucket >= ? AND bucket < ?
		GROUP BY transaction
		HAVING (countMerge(lcp_count) + countMerge(inp_count) + countMerge(cls_count)
			+ countMerge(fcp_count) + countMerge(ttfb_count)) > 0
		ORDER BY (countMerge(lcp_count) + countMerge(inp_count) + countMerge(cls_count)
			+ countMerge(fcp_count) + countMerge(ttfb_count)) DESC, transaction
		LIMIT ?`,
		uint64(projectID), from, to, k)
	if err != nil {
		return nil, fmt.Errorf("trace: top vital pages: %w", err)
	}
	defer rows.Close()
	return scanStrings(rows, "trace: top vital pages")
}

// scanStrings собирает одноколоночный результат в срез строк (общий хвост
// TopEndpointsByTraffic/TopVitalPages).
func scanStrings(rows driver.Rows, where string) ([]string, error) {
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("%s: scan: %w", where, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	return out, nil
}

// msSample собирает RegressionSample из микросекундного значения (p95 dur) с
// переводом в миллисекунды. При нуле замеров Value 0 (quantilesMerge пустого
// состояния — NaN, его в Decide пускать нельзя).
func msSample(us float64, cnt uint64) RegressionSample {
	if cnt == 0 {
		return RegressionSample{Value: 0, Samples: 0}
	}
	return RegressionSample{Value: us / 1000, Samples: int(cnt)}
}

// valueSample — как msSample, но без конвертации единиц (web-vital'ы уже в мс,
// CLS безразмерный). Только для вайтал-запросов (Recent/BaselineVitalP75[s]) —
// путь duration идёт исключительно через msSample. Раньше пакетные запросы
// эндпойнтов (RecentEndpointP95s, BaselineEndpointP95s) тоже звали valueSample
// и отдавали в Decide микросекунды дур, которые сравнивались с миллисекундными
// порогами регрессии — отсюда завышение длительности в тысячу раз и
// фактически выключенный абсолютный пол (см. Decide в regression.go). Два
// варианта одного запроса с разными единицами были корнем; теперь единица
// duration держится единственной точкой конвертации — msSample.
func valueSample(v float64, cnt uint64) RegressionSample {
	if cnt == 0 {
		return RegressionSample{Value: 0, Samples: 0}
	}
	return RegressionSample{Value: v, Samples: int(cnt)}
}

// usFromFloat округляет квантиль (Float64 из quantiles/quantilesMerge) до
// микросекунд с насыщением на границах UInt32.
func usFromFloat(v float64) uint32 {
	// NaN проскакивает оба сравнения ниже (NaN <= 0 и NaN > MaxUint32 оба false),
	// оставляя implementation-defined uint32(NaN); отсекаем явно.
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	r := math.Round(v)
	if r > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(r)
}
