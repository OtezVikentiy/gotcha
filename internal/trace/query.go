package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
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
}

// LatencyPoint — точка временного ряда латентности эндпойнта: T — начало
// интервала (UTC), P50/P95 — перцентили в микросекундах, Count — число
// транзакций в интервале (0, если их не было).
type LatencyPoint struct {
	T   time.Time
	P50 uint32
	P95 uint32
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
			quantilesMerge(0.5, 0.75, 0.95, 0.99)(dur) AS q
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
		if err := rows.Scan(&s.Transaction, &s.Count, &failures, &qs); err != nil {
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

// apdexByTransaction считает Apdex каждого эндпойнта из сырых transactions.
// satisfied = duration ≤ T·1000 µs, tolerating = T·1000 < duration ≤ 4·T·1000;
// Apdex = (satisfied + tolerating/2) / total = (satisfied + within4T) / (2·total).
func (q *Query) apdexByTransaction(ctx context.Context, projectID int64, from, to time.Time, environment string, apdexT int) (map[string]float64, error) {
	satUS := uint32(apdexT) * 1000
	tolUS := satUS * 4

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
	endUnix := (toUnix / stepSec) * stepSec
	if toUnix%stepSec > 0 {
		endUnix += stepSec
	}

	var out []LatencyPoint
	for curUnix := startUnix; curUnix <= endUnix; curUnix += stepSec {
		p := byBucket[curUnix] // zero LatencyPoint if bucket has no transactions
		p.T = time.Unix(curUnix, 0).UTC()
		out = append(out, p)
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

// usFromFloat округляет квантиль (Float64 из quantiles/quantilesMerge) до
// микросекунд с насыщением на границах UInt32.
func usFromFloat(v float64) uint32 {
	if v <= 0 {
		return 0
	}
	r := math.Round(v)
	if r > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(r)
}
