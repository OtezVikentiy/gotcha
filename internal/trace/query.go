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

// значения только через ? — никогда не конкатенировать в текст запроса.
// параметр environment: пустая строка = без фильтра по окружению (везде, где он есть).
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

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
	// список, не одно значение: строка агрегирует все окружения сразу,
	// разбивка задвоила бы эндпойнт с разными перцентилями.
	Environments []string
}

// T — начало интервала в UTC; P50/P95 — микросекунды; Count=0, если транзакций не было.
type LatencyPoint struct {
	T     time.Time
	P50   uint32
	P95   uint32
	Count uint64
}

// UpperUS/Count — верхняя граница корзины в микросекундах и число транзакций в ней.
type DurationBucket struct {
	UpperUS uint32
	Count   uint64
}

type TraceRow struct {
	TraceID    string
	DurationUS uint32
	Timestamp  time.Time
	Status     string
}

// StartUS — смещение от начала трейса, не абсолютное время; оба поля в микросекундах.
type SpanRow struct {
	SpanID       string
	ParentSpanID string
	Op           string
	Description  string
	Status       string
	StartUS      uint32
	DurationUS   uint32
}

// потолок против аномальной кардинальности transaction (CardinalityGuard даёт до
// GOTCHA_CARDINALITY_LIMIT новых имён за окно, по умолчанию 10000/час, и сбрасывает счётчик
// на границе часа — за широкое окно накопится больше). Превышение возвращается truncated=true,
// а не тихим обрезанием: вызывающий обязан честно сообщить о неполноте, не выдать её за полный список.
const endpointsRowCap = 20000

// apdex считается отдельным запросом по сырым transactions — MV хранит только
// квантили, не пороговые счётчики; apdexT<=0 — без Apdex.
func (q *Query) Endpoints(ctx context.Context, projectID int64, from, to time.Time, environment string, apdexT int) ([]EndpointStat, bool, error) {
	where := "project_id = ? AND bucket >= ? AND bucket < ?"
	args := []any{uint64(projectID), from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}
	args = append(args, endpointsRowCap+1)

	rows, err := q.conn.Query(ctx, `
		SELECT transaction,
			countMerge(cnt) AS c,
			countMerge(failures) AS f,
			quantilesMerge(0.5, 0.75, 0.95, 0.99)(dur) AS q,
			arraySort(groupUniqArray(environment)) AS envs
		FROM transactions_5m
		WHERE `+where+`
		GROUP BY transaction
		ORDER BY c DESC, transaction
		LIMIT ?
		SETTINGS max_execution_time = 10`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("trace: endpoints: %w", err)
	}
	defer rows.Close()

	periodMin := to.Sub(from).Minutes()
	var out []EndpointStat
	for rows.Next() {
		var s EndpointStat
		var failures uint64
		var qs []float64
		if err := rows.Scan(&s.Transaction, &s.Count, &failures, &qs, &s.Environments); err != nil {
			return nil, false, fmt.Errorf("trace: endpoints: scan: %w", err)
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
		return nil, false, fmt.Errorf("trace: endpoints: %w", err)
	}

	truncated := len(out) > endpointsRowCap
	if truncated {
		out = out[:endpointsRowCap]
	}

	if apdexT > 0 {
		transactions := make([]string, len(out))
		for i, s := range out {
			transactions[i] = s.Transaction
		}
		apdex, err := q.apdexByTransaction(ctx, projectID, from, to, environment, apdexT, transactions)
		if err != nil {
			return nil, false, err
		}
		for i := range out {
			out[i].ApdexScore = apdex[out[i].Transaction]
		}
	}
	return out, truncated, nil
}

type Dependency struct {
	Kind      string // database | cache | http
	Target    string // postgresql | redis | api.stripe.com | ...
	Calls     int64
	Reads     int64 // вызовы с глаголом чтения (SELECT/GET/HGET/…), см. *ReadVerbs
	Writes    int64 // вызовы с глаголом записи (INSERT/POST/SET/…), см. *WriteVerbs
	P50US     uint32
	P95US     uint32
	ErrorRate float64 // доля спанов со status != 'ok'
}

type DataDirection string

const (
	DirectionNone DataDirection = ""     // ни чтения, ни записи (BEGIN/COMMIT, неизвестные команды)
	DirectionIn   DataDirection = "in"   // только чтение
	DirectionOut  DataDirection = "out"  // только запись
	DirectionBoth DataDirection = "both" // чтение и запись
)

func (d Dependency) Direction() DataDirection {
	switch {
	case d.Reads > 0 && d.Writes > 0:
		return DirectionBoth
	case d.Reads > 0:
		return DirectionIn
	case d.Writes > 0:
		return DirectionOut
	}
	return DirectionNone
}

// подставляются в SQL через verbList; те же списки читают UI и документация —
// менять только синхронно.
const (
	SQLReadVerbs  = "SELECT WITH SHOW EXPLAIN DESCRIBE DESC"
	SQLWriteVerbs = "INSERT UPDATE DELETE MERGE REPLACE UPSERT CREATE ALTER DROP TRUNCATE COPY"

	CacheReadVerbs = "GET GETS MGET HGET HGETALL HMGET HEXISTS HLEN HKEYS HVALS EXISTS KEYS SCAN SSCAN HSCAN ZSCAN " +
		"LRANGE LLEN LINDEX SMEMBERS SISMEMBER SCARD ZRANGE ZREVRANGE ZRANGEBYSCORE ZSCORE ZCARD ZCOUNT ZRANK " +
		"TTL PTTL TYPE STRLEN GETRANGE PING"
	CacheWriteVerbs = "SET SETEX SETNX PSETEX MSET MSETNX GETSET GETDEL DEL UNLINK INCR INCRBY INCRBYFLOAT DECR DECRBY " +
		"APPEND HSET HMSET HDEL HINCRBY LPUSH RPUSH LPOP RPOP LSET LREM LTRIM SADD SREM SPOP ZADD ZREM ZINCRBY " +
		"EXPIRE PEXPIRE EXPIREAT PERSIST RENAME PUBLISH XADD FLUSHDB FLUSHALL ADD CAS PREPEND TOUCH FLUSH_ALL"

	HTTPReadVerbs  = "GET HEAD OPTIONS"
	HTTPWriteVerbs = "POST PUT PATCH DELETE"
)

// экранирования нет — безопасно только для констант выше, не для внешнего ввода.
func verbList(verbs string) string {
	fields := strings.Fields(verbs)
	for i, v := range fields {
		fields[i] = "'" + v + "'"
	}
	return strings.Join(fields, ",")
}

// по сырым spans, не по MV — окно защищено max_execution_time + LIMIT и дефолт-окном на web.
// порт у server.address срезается regex `:[0-9]+$` — может задеть хвост IPv6-литерала.
func (q *Query) Dependencies(ctx context.Context, projectID int64, from, to time.Time, limit int) ([]Dependency, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := q.conn.Query(ctx, `
		SELECT kind, target, count() AS c, countIf(status != 'ok') AS f,
			countIf(verbClass = 'r') AS r, countIf(verbClass = 'w') AS w,
			quantiles(0.5, 0.95)(duration_us) AS q
		FROM (
			SELECT
				multiIf(op = 'db' OR startsWith(op,'db.sql') OR op = 'db.query', 'database',
						startsWith(op,'db.'), 'cache',
						'http') AS kind,
				upper(if(kind = 'http',
					coalesce(
						nullIf(JSONExtractString(data,'http.request.method'),''),
						nullIf(JSONExtractString(data,'http.method'),''),
						extract(description, '^\\s*([A-Za-z_]+)')),
					coalesce(
						nullIf(JSONExtractString(data,'db.operation.name'),''),
						nullIf(JSONExtractString(data,'db.operation'),''),
						extract(description, '^\\s*([A-Za-z_]+)')))) AS verb,
				multiIf(
					kind = 'database' AND verb IN (`+verbList(SQLReadVerbs)+`), 'r',
					kind = 'database' AND verb IN (`+verbList(SQLWriteVerbs)+`), 'w',
					kind = 'cache' AND verb IN (`+verbList(CacheReadVerbs)+`), 'r',
					kind = 'cache' AND verb IN (`+verbList(CacheWriteVerbs)+`), 'w',
					kind = 'http' AND verb IN (`+verbList(HTTPReadVerbs)+`), 'r',
					kind = 'http' AND verb IN (`+verbList(HTTPWriteVerbs)+`), 'w',
					'') AS verbClass,
				multiIf(
					op = 'db' OR startsWith(op,'db.sql') OR op = 'db.query',
						coalesce(nullIf(JSONExtractString(data,'db.system'),''), nullIf(JSONExtractString(data,'db.system.name'),''), 'database'),
					startsWith(op,'db.'),
						arrayElement(splitByChar('.', op), 2),
					coalesce(
						nullIf(replaceRegexpOne(JSONExtractString(data,'server.address'), ':[0-9]+$', ''),''),
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
		var calls, failures, reads, writes uint64
		var qs []float64
		if err := rows.Scan(&d.Kind, &d.Target, &calls, &failures, &reads, &writes, &qs); err != nil {
			return nil, fmt.Errorf("trace: dependencies: scan: %w", err)
		}
		d.Calls = int64(calls)
		d.Reads = int64(reads)
		d.Writes = int64(writes)
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

// duration_us в CH — uint32; без клампа satUS/tolUS переполнились бы при
// apdexT выше ~4.29M мс (валидация настройки верхней границы не ставит).
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

// apdex = (satisfied + tolerating/2) / total = (satisfied + within4T) / (2·total).
// transactions ограничивает GROUP BY именами, уже отобранными Endpoints — независимого
// потолка кардинальности здесь не нужно, он унаследован от endpointsRowCap.
func (q *Query) apdexByTransaction(ctx context.Context, projectID int64, from, to time.Time, environment string, apdexT int, transactions []string) (map[string]float64, error) {
	out := make(map[string]float64)
	if len(transactions) == 0 {
		return out, nil
	}
	satUS, tolUS := apdexBoundsUS(apdexT)

	where := "project_id = ? AND transaction IN ? AND timestamp >= ? AND timestamp < ?"
	args := []any{satUS, tolUS, uint64(projectID), transactions, from, to}
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
		GROUP BY transaction
		SETTINGS max_execution_time = 10`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: apdex: %w", err)
	}
	defer rows.Close()

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

// точки идут от from до to ВКЛЮЧИТЕЛЬНО, пропуски заполняются нулями; при step,
// кратном 5м, ряд собирается из MV transactions_5m, иначе из сырых transactions.
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
	// endUnix — начало последней корзины, СОДЕРЖАЩЕЙ to (не следующей): запрос
	// фильтрует ts < to, дальше в ней данных быть не может.
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

// один запрос вместо N (WHERE transaction IN ?), сетка и заполнение пропусков
// как в EndpointLatency; несуществующий в БД transaction получает ряд из нулей.
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

// UpperUS корзины i = (i+1)·width; пустой результат — nil, не []DurationBucket{}.
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

// дефолт только для новой установки — реальный TTL задаёт GOTCHA_SPAN_RETENTION_DAYS,
// хранится в web.Handler.SpanRetentionDays; в хендлерах сравнивать с ним, не с этой константой.
const SpanRetentionDays = 30

// с запасом больше waterfallMaxRows=200 (internal/web) — иначе усечение
// случилось бы до отрисовки, а не после.
const traceSpanLimit = 5000

// root — спан без родителя, а если такого нет — первый по времени; spans == nil
// означает, что трейс не найден.
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

// предохранитель от кривых данных — evidence.span_ids уже ограничен ≤10.
const maxOffendingSpans = 10

// полный текст (Title проблемы обрезан/нормализован); Data может нести
// code.filepath/code.lineno/code.function, db.system, если их прислал SDK.
type SpanDetail struct {
	SpanID      string
	Op          string
	Description string
	DurationUS  uint32
	Data        map[string]string
}

// пустой traceID/список, истёкший TTL или отсутствие спанов — nil без ошибки,
// страница проблемы рисуется и без примера.
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

// вложенные объекты/массивы пропускаются — нужны только скалярные ключи вроде
// code.filepath/code.lineno; битый/пустой JSON — nil.
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

// trace_id — клиентское значение и не скопирован от project_id: чужой проект
// может прислать ту же строку, поэтому список отдаём целиком, а не первую
// попавшуюся строку — выбор среди них остаётся за вызывающим (доступ пользователя).
// Порядок ЗНАЧИМ, не порядок хранения: если trace_id совпал в нескольких
// проектах, ДОСТУПНЫХ ОДНОМУ пользователю, resolveTraceProject (web/trace.go)
// берёт первый доступный — здесь это проект с самой свежей транзакцией по
// этому id, а не произвольная строка ClickHouse.
func (q *Query) ProjectsForTrace(ctx context.Context, traceID string) ([]int64, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT project_id FROM transactions WHERE trace_id = ?
		GROUP BY project_id ORDER BY max(timestamp) DESC, project_id ASC`, traceID)
	if err != nil {
		return nil, fmt.Errorf("trace: projects for trace: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var pid uint64
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("trace: projects for trace: scan: %w", err)
		}
		out = append(out, int64(pid))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: projects for trace: %w", err)
	}
	return out, nil
}

// отличает настоящее истечение TTL спанов (транзакция ещё жива, её TTL длиннее)
// от потери спанов на буфере писателя под нагрузкой — обе выглядят как spans==nil.
func (q *Query) TransactionTimestamp(ctx context.Context, projectID int64, traceID string) (time.Time, bool, error) {
	row := q.conn.QueryRow(ctx, `
		SELECT timestamp FROM transactions WHERE project_id = ? AND trace_id = ? LIMIT 1`,
		uint64(projectID), traceID)
	var ts time.Time
	if err := row.Scan(&ts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("trace: transaction timestamp: %w", err)
	}
	return ts.UTC(), true, nil
}

// в отличие от ProjectsForTrace: project_id — префикс PK transactions, запрос
// прунит гранулы до проекта вместо обхода партиций всех проектов.
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

// P75 — мс, кроме CLS (безразмерный); Rating — "good"|"needs-improvement"|"poor"
// или "", если замеров нет (Count == 0).
type Vital struct {
	Name   string
	P75    float64
	Rating string
	Count  uint64
}

// Count — замеры LCP (по нему сортируется список страниц).
type PageVitals struct {
	Transaction string
	LCP         Vital
	INP         Vital
	CLS         Vital
	Count       uint64
	// список, не одно значение — как в EndpointStat.Environments.
	Environments []string
}

// T — UTC; в отличие от LatencyPoint, пустые корзины (без замеров) в ряд не
// попадают, а не зануляются.
type VitalPoint struct {
	T   time.Time
	P75 float64
}

// мс, кроме CLS (безразмерный); good включительна, выше poor — poor, между — needs-improvement.
const (
	lcpGood, lcpPoor   = 2500.0, 4000.0
	inpGood, inpPoor   = 200.0, 500.0
	clsGood, clsPoor   = 0.1, 0.25
	fcpGood, fcpPoor   = 1800.0, 3000.0
	ttfbGood, ttfbPoor = 800.0, 1800.0
)

const webVitalsPageLimit = 200

// p75 == 0 для известного vital — валидный "good", не признак отсутствия
// данных; это проверяет вызывающий по Count.
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

// имя подставляется в SQL как колонка (не значение → не через ?), поэтому
// проверяется белым списком до конкатенации.
func vitalKnown(name string) bool {
	switch name {
	case "lcp", "inp", "cls", "fcp", "ttfb":
		return true
	}
	return false
}

// при нуле замеров quantilesMerge пустого состояния возвращает NaN — не читаем,
// P75/Rating остаются нулевыми.
func makeVital(name string, p75 []float64, count uint64) Vital {
	v := Vital{Name: name, Count: count}
	if count > 0 && len(p75) > 0 {
		v.P75 = p75[0]
		v.Rating = Rating(name, v.P75)
	}
	return v
}

// HAVING отсекает транзакции без единого замера LCP/INP/CLS — MV агрегирует ВСЕ
// транзакции проекта, включая чистые API-эндпойнты без measurements.
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

func (q *Query) PageVitalsOne(ctx context.Context, projectID int64, transaction string, from, to time.Time, environment string) (lcp, inp, cls, fcp, ttfb Vital, err error) {
	where := "project_id = ? AND transaction = ? AND bucket >= ? AND bucket < ?"
	args := []any{uint64(projectID), transaction, from, to}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	var lcpP, inpP, clsP, fcpP, ttfbP []float64
	var lcpC, inpC, clsC, fcpC, ttfbC uint64
	// без GROUP BY — всегда ровно одна строка (нулевая при отсутствии замеров),
	// ErrNoRows не бывает.
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

// Value уже в мс (CLS безразмерный); name — из vitalKnown, безопасно как имя колонки.
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

// HAVING cnt>0 — иначе quantilesMerge пустого состояния дал бы NaN и испортил медиану.
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

// без клампа window_minutes ≥ недели вырождает modulo(...) в always-true — full scan вместо слота.
// клампим и здесь, не только в валидации: в БД может быть конфиг без этой границы.
const maxSeasonalWindowMinutes = 1440

// k>=1 исключает текущую (неполную) неделю — иначе недобранный час занижает базу;
// from сдвинут на -windowMinutes, иначе окно самой старой недели (k=weeks) ушло бы за from.
func (q *Query) SeasonalBaselineEndpointP95s(ctx context.Context, projectID int64, transactions []string, windowMinutes, weeks int, now time.Time) (map[string]RegressionSample, error) {
	out := make(map[string]RegressionSample, len(transactions))
	if len(transactions) == 0 {
		return out, nil
	}
	if windowMinutes > maxSeasonalWindowMinutes {
		windowMinutes = maxSeasonalWindowMinutes // см. maxSeasonalWindowMinutes: держим слот у́же недели
	}
	weekSec := int64(7 * 24 * 3600)
	winSec := int64(windowMinutes) * 60
	nowSec := int64(now.Unix()) // epoch считаем в Go, а не toUInt32(param) в SQL
	from := now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour).Add(-time.Duration(windowMinutes) * time.Minute)
	rows, err := q.conn.Query(ctx, `
		SELECT transaction, quantileExact(0.5)(weekly) AS base, sum(cnt) AS total
		FROM (
			SELECT transaction,
				intDiv(? - toUInt32(bucket), ?) AS k,
				quantilesMerge(0.95)(dur)[1] AS weekly,
				countMerge(cnt) AS cnt
			FROM transactions_5m
			WHERE project_id = ? AND transaction IN ?
				AND bucket >= ? AND bucket < ?
				AND modulo(? - toUInt32(bucket), ?) < ?
			GROUP BY transaction, k
		)
		WHERE k >= 1
		GROUP BY transaction`,
		nowSec, weekSec, uint64(projectID), transactions, from, now, nowSec, weekSec, winSec)
	if err != nil {
		return nil, fmt.Errorf("trace: seasonal baseline endpoint p95s: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var med float64
		var total uint64
		if err := rows.Scan(&name, &med, &total); err != nil {
			return nil, fmt.Errorf("trace: seasonal baseline endpoint p95s scan: %w", err)
		}
		out[name] = msSample(med, total)
	}
	return out, rows.Err()
}

type VitalKey struct {
	Transaction string
	Metric      string
}

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

// HAVING тут не годится — строка дня общая для всех метрик; пустые по метрике
// дни отбрасывает quantileExactIf по её счётчику.
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

// то же выравнивание недели, что в SeasonalBaselineEndpointP95s, но по всем метрикам сразу.
func (q *Query) SeasonalBaselineVitalP75s(ctx context.Context, projectID int64, transactions, metrics []string, windowMinutes, weeks int, now time.Time) (map[VitalKey]RegressionSample, error) {
	out := make(map[VitalKey]RegressionSample, len(transactions)*len(metrics))
	if len(transactions) == 0 || len(metrics) == 0 {
		return out, nil
	}
	var inner, outer []string
	for _, m := range metrics {
		if !vitalKnown(m) {
			return nil, fmt.Errorf("trace: seasonal baseline vital p75s: unknown vital %q", m)
		}
		inner = append(inner,
			fmt.Sprintf("quantilesMerge(0.75)(%s)[1] AS %s_daily", m, m),
			fmt.Sprintf("countMerge(%s_count) AS %s_cnt", m, m))
		outer = append(outer,
			fmt.Sprintf("quantileExactIf(0.5)(%s_daily, %s_cnt > 0) AS %s_base", m, m, m),
			fmt.Sprintf("sum(%s_cnt) AS %s_total", m, m))
	}
	if windowMinutes > maxSeasonalWindowMinutes {
		windowMinutes = maxSeasonalWindowMinutes // см. maxSeasonalWindowMinutes: держим слот у́же недели
	}
	weekSec := int64(7 * 24 * 3600)
	winSec := int64(windowMinutes) * 60
	nowSec := int64(now.Unix())
	from := now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour).Add(-time.Duration(windowMinutes) * time.Minute)
	rows, err := q.conn.Query(ctx, `
		SELECT transaction, `+strings.Join(outer, ", ")+`
		FROM (
			SELECT transaction,
				intDiv(? - toUInt32(bucket), ?) AS k, `+strings.Join(inner, ", ")+`
			FROM web_vitals_5m
			WHERE project_id = ? AND transaction IN ?
				AND bucket >= ? AND bucket < ?
				AND modulo(? - toUInt32(bucket), ?) < ?
			GROUP BY transaction, k
		)
		WHERE k >= 1
		GROUP BY transaction`,
		nowSec, weekSec, uint64(projectID), transactions, from, now, nowSec, weekSec, winSec)
	if err != nil {
		return nil, fmt.Errorf("trace: seasonal baseline vital p75s: %w", err)
	}
	defer rows.Close()
	if err := scanVitalRows(rows, metrics, out); err != nil {
		return nil, fmt.Errorf("trace: seasonal baseline vital p75s: %w", err)
	}
	return out, rows.Err()
}

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

// HAVING отсекает эндпойнты без единого замера vital'а — MV агрегирует и чистые
// API без measurements.
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

// при нуле замеров Value=0 — quantilesMerge пустого состояния дал бы NaN,
// недопустимый в Decide.
func msSample(us float64, cnt uint64) RegressionSample {
	if cnt == 0 {
		return RegressionSample{Value: 0, Samples: 0}
	}
	return RegressionSample{Value: us / 1000, Samples: int(cnt)}
}

// только для веб-виталов (уже в мс); duration всегда через msSample — второй
// путь смешивает единицы.
func valueSample(v float64, cnt uint64) RegressionSample {
	if cnt == 0 {
		return RegressionSample{Value: 0, Samples: 0}
	}
	return RegressionSample{Value: v, Samples: int(cnt)}
}

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

// живёт здесь, не в slo — иначе цикл slo→trace→slo; T в UTC.
type CountBucket struct {
	T           time.Time
	Good, Total uint64
}

// step должен быть кратен 5 минутам (гранулярность MV, не проверяется в коде).
func (q *Query) GoodTotalBuckets(ctx context.Context, projectID int64, transaction, environment string, from, to time.Time, step time.Duration) ([]CountBucket, error) {
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("trace: good/total buckets: step must be at least one second, got %s", step)
	}

	where := "project_id = ? AND bucket >= ? AND bucket < ?"
	args := []any{stepSec, uint64(projectID), from, to}
	if transaction != "" {
		where += " AND transaction = ?"
		args = append(args, transaction)
	}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(bucket, INTERVAL ? second) AS t,
			countMerge(cnt) AS total,
			countMerge(failures) AS failures
		FROM transactions_5m
		WHERE `+where+`
		GROUP BY t
		ORDER BY t`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: good/total buckets: %w", err)
	}
	defer rows.Close()

	var out []CountBucket
	for rows.Next() {
		var t time.Time
		var total, failures uint64
		if err := rows.Scan(&t, &total, &failures); err != nil {
			return nil, fmt.Errorf("trace: good/total buckets: scan: %w", err)
		}
		good := uint64(0)
		if total > failures { // failures ≤ total всегда, но вычитание uint64 защищаем
			good = total - failures
		}
		out = append(out, CountBucket{T: t.UTC(), Good: good, Total: total})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: good/total buckets: %w", err)
	}
	return out, nil
}

func (q *Query) LatencyGoodBuckets(ctx context.Context, projectID int64, transaction, environment string, thresholdUS uint64, from, to time.Time, step time.Duration) ([]CountBucket, error) {
	stepSec := int64(step / time.Second)
	if stepSec <= 0 {
		return nil, fmt.Errorf("trace: latency good buckets: step must be at least one second, got %s", step)
	}

	where := "project_id = ? AND timestamp >= ? AND timestamp < ?"
	args := []any{stepSec, thresholdUS, uint64(projectID), from, to}
	if transaction != "" {
		where += " AND transaction = ?"
		args = append(args, transaction)
	}
	if environment != "" {
		where += " AND environment = ?"
		args = append(args, environment)
	}

	rows, err := q.conn.Query(ctx, `
		SELECT toStartOfInterval(timestamp, INTERVAL ? second) AS t,
			count() AS total,
			countIf(duration_us <= ?) AS good
		FROM transactions
		WHERE `+where+`
		GROUP BY t
		ORDER BY t
		SETTINGS max_execution_time = 10`, args...)
	if err != nil {
		return nil, fmt.Errorf("trace: latency good buckets: %w", err)
	}
	defer rows.Close()

	var out []CountBucket
	for rows.Next() {
		var t time.Time
		var total, good uint64
		if err := rows.Scan(&t, &total, &good); err != nil {
			return nil, fmt.Errorf("trace: latency good buckets: scan: %w", err)
		}
		out = append(out, CountBucket{T: t.UTC(), Good: good, Total: total})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: latency good buckets: %w", err)
	}
	return out, nil
}
