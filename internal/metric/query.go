package metric

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type Query struct {
	conn driver.Conn
	// nil у обычного Query: страничные чтения делают по одному Series/Aggregate
	// на запрос, кешировать нечего.
	types *TypeCache
}

// Кеш на один тик оценщика: тип не зависит от хоста, но окно [from,to) в ключе
// двигается — долгоживущий кеш давал бы неверный ответ для новых окон.
type TypeCache struct {
	mu sync.Mutex
	m  map[typeCacheKey]typeCacheValue
}

type typeCacheKey struct {
	projectID int64
	name      string
	from, to  int64
}

type typeCacheValue struct {
	typ         string
	monotonic   bool
	temporality string
}

func NewTypeCache() *TypeCache {
	return &TypeCache{m: map[typeCacheKey]typeCacheValue{}}
}

// Копия, не поле общего экземпляра: *Query делят веб-хендлеры и оценщики,
// кеш одного прохода не должен стать их общим состоянием.
func (q *Query) WithTypeCache(c *TypeCache) *Query {
	cp := *q
	cp.types = c
	return &cp
}

// Потолок строк для Labels: arrayJoin(mapKeys(attributes)) раздувает вход,
// без него миллионы точек дали бы сотни миллионов промежуточных строк.
const labelSampleRows = 200_000

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

type MetricInfo struct {
	Name string
	Type string
	Unit string
}

type Point struct {
	T time.Time
	V float64
}

// Пустой Key — без фильтра.
type LabelMatcher struct {
	Key   string
	Value string
}

func (q *Query) ListMetrics(ctx context.Context, projectID int64, environment string) ([]MetricInfo, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT name, any(type), any(unit)
		FROM metric_points
		WHERE project_id = ? AND (? = '' OR environment = ?)
		GROUP BY name
		ORDER BY name`,
		projectID, environment, environment)
	if err != nil {
		return nil, fmt.Errorf("metric: list metrics: %w", err)
	}
	defer rows.Close()
	var out []MetricInfo
	for rows.Next() {
		var m MetricInfo
		if err := rows.Scan(&m.Name, &m.Type, &m.Unit); err != nil {
			return nil, fmt.Errorf("metric: list metrics scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// (project_id, name) — префикс первичного ключа metric_points, LIMIT 1
// короткозамыкает на первой гранле — не ListMetrics с полным 30-дневным сканом.
func (q *Query) MetricInfoByName(ctx context.Context, projectID int64, name string) (MetricInfo, bool, error) {
	row := q.conn.QueryRow(ctx, `
		SELECT name, type, unit FROM metric_points
		WHERE project_id = ? AND name = ? LIMIT 1`,
		projectID, name)
	var m MetricInfo
	if err := row.Scan(&m.Name, &m.Type, &m.Unit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MetricInfo{}, false, nil
		}
		return MetricInfo{}, false, fmt.Errorf("metric: info by name: %w", err)
	}
	return m, true, nil
}

// Самый тяжёлый запрос: arrayJoin(mapKeys) раздувает вход, поэтому лимит.
// LIMIT без ORDER BY — не обязательно свежие точки, для дропдауна хватает.
func (q *Query) Labels(ctx context.Context, projectID int64, name string, from, to time.Time) (map[string][]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT k, groupUniqArray(20)(v) FROM (
			SELECT
				arrayJoin(mapKeys(attributes)) AS k,
				attributes[k] AS v
			FROM (
				SELECT attributes FROM metric_points
				WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
				LIMIT ?
			)
		)
		GROUP BY k
		ORDER BY k`,
		projectID, name, from, to, labelSampleRows)
	if err != nil {
		return nil, fmt.Errorf("metric: labels: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var k string
		var vs []string
		if err := rows.Scan(&k, &vs); err != nil {
			return nil, fmt.Errorf("metric: labels scan: %w", err)
		}
		out[k] = vs
	}
	return out, rows.Err()
}

// Окно [from,to] ограничивает скан: без него DISTINCT читал бы весь
// 30-дневный ретеншн метрики.
func (q *Query) Environments(ctx context.Context, projectID int64, name string, from, to time.Time) ([]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT environment FROM metric_points
		WHERE project_id = ? AND name = ? AND environment != '' AND ts >= ? AND ts < ?
		ORDER BY environment`,
		projectID, name, from, to)
	if err != nil {
		return nil, fmt.Errorf("metric: environments: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("metric: environments scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Ограничено окном вызывающего: без него any() читал бы весь ретеншн на
// каждый Series/Aggregate. Пустое окно — тип не важен, дальше вернётся пусто.
func (q *Query) metricType(ctx context.Context, projectID int64, name string, from, to time.Time) (typ string, monotonic bool, temporality string, err error) {
	key := typeCacheKey{projectID: projectID, name: name, from: from.UnixNano(), to: to.UnixNano()}
	if q.types != nil {
		q.types.mu.Lock()
		v, ok := q.types.m[key]
		q.types.mu.Unlock()
		if ok {
			return v.typ, v.monotonic, v.temporality, nil
		}
	}
	row := q.conn.QueryRow(ctx, `
		SELECT any(type), any(monotonic), any(temporality)
		FROM metric_points WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?`,
		projectID, name, from, to)
	var mono uint8
	if err := row.Scan(&typ, &mono, &temporality); err != nil {
		// При empty_result_for_aggregation_by_empty_set=1 вернёт ErrNoRows — трактуем
		// как «тип не важен», Series/Aggregate по пустому окну и так вернут пусто.
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, "", nil
		}
		return "", false, "", fmt.Errorf("metric: metric type: %w", err)
	}
	// Кешируем только успех: ошибка — состояние ClickHouse, не метрики; кеш на
	// весь проход иначе разнёс бы одну неудачу на все хосты проекта.
	if q.types != nil {
		q.types.mu.Lock()
		q.types.m[key] = typeCacheValue{typ: typ, monotonic: mono == 1, temporality: temporality}
		q.types.mu.Unlock()
	}
	return typ, mono == 1, temporality, nil
}

// Стратегия по типу: sum monotonic cumulative → rate, histogram+перцентиль →
// интерполяция, иначе — agg. matchers — AND по attributes, host — отдельная колонка.
func (q *Query) Series(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, agg string, from, to time.Time, step time.Duration) ([]Point, error) {
	matchers = compactMatchers(matchers)
	typ, monotonic, temporality, err := q.metricType(ctx, projectID, name, from, to)
	if err != nil {
		return nil, err
	}
	stepSec := int64(step.Seconds())
	if stepSec < 1 {
		stepSec = 1
	}

	switch {
	case typ == "histogram" && isPercentile(agg):
		return q.histogramSeries(ctx, projectID, name, environment, host, matchers, agg, from, to, stepSec)
	case typ == "sum" && monotonic && temporality == "cumulative":
		return q.rateSeries(ctx, projectID, name, environment, host, matchers, from, to, stepSec)
	default:
		return q.scalarSeries(ctx, projectID, name, environment, host, matchers, typ, agg, from, to, stepSec)
	}
}

// Для histogram+avg — sum(value)/sum(count) (среднее наблюдение).
func (q *Query) scalarSeries(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, typ, agg string, from, to time.Time, stepSec int64) ([]Point, error) {
	aggExpr := scalarAggExpr(typ, agg)
	sql := fmt.Sprintf(`
		SELECT toStartOfInterval(ts, INTERVAL %d second) AS b, %s
		FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  AND (? = '' OR host = ?)
		  %s
		GROUP BY b ORDER BY b`, stepSec, aggExpr, matchersClause(matchers))
	args := []any{projectID, name, from, to, environment, environment, host, host}
	args = appendMatchersArgs(args, matchers)
	return q.scanPoints(ctx, sql, args)
}

// max(value) по бакету — сырые значения кумулятивного счётчика, общие для
// rateSeries (скорость) и aggregateIncrease (прирост).
func (q *Query) cumulativeBuckets(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, from, to time.Time, stepSec int64) ([]Point, error) {
	sql := fmt.Sprintf(`
		SELECT toStartOfInterval(ts, INTERVAL %d second) AS b, max(value)
		FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  AND (? = '' OR host = ?)
		  %s
		GROUP BY b ORDER BY b`, stepSec, matchersClause(matchers))
	args := []any{projectID, name, from, to, environment, environment, host, host}
	args = appendMatchersArgs(args, matchers)
	return q.scanPoints(ctx, sql, args)
}

// Разность соседних кумулятивных точек / шаг; отрицательная разность
// (сброс счётчика) → 0.
func (q *Query) rateSeries(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, from, to time.Time, stepSec int64) ([]Point, error) {
	cum, err := q.cumulativeBuckets(ctx, projectID, name, environment, host, matchers, from, to, stepSec)
	if err != nil {
		return nil, err
	}
	if len(cum) < 2 {
		return nil, nil
	}
	out := make([]Point, 0, len(cum)-1)
	for i := 1; i < len(cum); i++ {
		delta := cum[i].V - cum[i-1].V
		if delta < 0 {
			delta = 0
		}
		// Делим на реальный интервал между точками, не на ширину корзины: GROUP BY
		// возвращает только непустые корзины — реже шага скрейп исказил бы скорость.
		gapSec := cum[i].T.Sub(cum[i-1].T).Seconds()
		if gapSec <= 0 {
			gapSec = float64(stepSec)
		}
		out = append(out, Point{T: cum[i].T, V: delta / gapSec})
	}
	return out, nil
}

// Сумма положительных разностей соседних точек за окно — прирост, не скорость.
// Сброс счётчика (delta < 0) обнуляет только сам переход, как в rateSeries.
// Считает сама ClickHouse (lagInFrame в окне) — без правила на потолок WindowSeconds
// суточное окно дало бы ~86400 секундных бакетов на КАЖДЫЙ тик оценщика, если бы их
// тащило и суммировало приложение; так — одна строка независимо от длины окна.
func (q *Query) aggregateIncrease(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, from, to time.Time) (float64, bool, error) {
	query := fmt.Sprintf(`
		SELECT count(), sum(d) FROM (
			SELECT greatest(value - lagInFrame(value, 1, value) OVER (ORDER BY b), 0) AS d
			FROM (
				SELECT toStartOfInterval(ts, INTERVAL 1 second) AS b, max(value) AS value
				FROM metric_points
				WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
				  AND (? = '' OR environment = ?)
				  AND (? = '' OR host = ?)
				  %s
				GROUP BY b
			)
		)`, matchersClause(matchers))
	args := []any{projectID, name, from, to, environment, environment, host, host}
	args = appendMatchersArgs(args, matchers)
	row := q.conn.QueryRow(ctx, query, args...)
	var cnt uint64
	var total float64
	if err := row.Scan(&cnt, &total); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil // пустое окно при empty_result_for_aggregation_by_empty_set=1
		}
		return 0, false, fmt.Errorf("metric: aggregate increase: %w", err)
	}
	if cnt < 2 {
		return 0, false, nil
	}
	return total, true, nil
}

func (q *Query) histogramSeries(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, agg string, from, to time.Time, stepSec int64) ([]Point, error) {
	sql := fmt.Sprintf(`
		SELECT toStartOfInterval(ts, INTERVAL %d second) AS b,
		       sumForEach(bucket_counts) AS bc,
		       any(explicit_bounds) AS eb
		FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  AND (? = '' OR host = ?)
		  %s
		GROUP BY b ORDER BY b`, stepSec, matchersClause(matchers))
	args := []any{projectID, name, from, to, environment, environment, host, host}
	args = appendMatchersArgs(args, matchers)
	rows, err := q.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("metric: histogram series: %w", err)
	}
	defer rows.Close()
	p := percentileValue(agg)
	var out []Point
	for rows.Next() {
		var b time.Time
		var bc []uint64
		var eb []float64
		if err := rows.Scan(&b, &bc, &eb); err != nil {
			return nil, fmt.Errorf("metric: histogram series scan: %w", err)
		}
		out = append(out, Point{T: b, V: histogramQuantile(bc, eb, p)})
	}
	return out, rows.Err()
}

// Существование, не значение — один запрос вместо Aggregate на каждое имя (тот тянет
// ещё metricType и, для monotonic-счётчиков, второй запрос на rate).
func (q *Query) NamesWithData(ctx context.Context, projectID int64, names []string, from, to time.Time) (map[string]bool, error) {
	out := make(map[string]bool, len(names))
	if len(names) == 0 {
		return out, nil
	}
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT name
		FROM metric_points
		WHERE project_id = ? AND name IN ? AND ts >= ? AND ts < ?
		SETTINGS max_execution_time = 10`,
		projectID, names, from, to)
	if err != nil {
		return nil, fmt.Errorf("metric: names with data: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("metric: names with data scan: %w", err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// Без бакетинга, единственное число за всё окно — для оценщика пороговых
// алертов («avg метрики за окно ⋛ порог»).
func (q *Query) Aggregate(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, agg string, from, to time.Time) (float64, bool, error) {
	matchers = compactMatchers(matchers)
	typ, monotonic, temporality, err := q.metricType(ctx, projectID, name, from, to)
	if err != nil {
		return 0, false, err
	}
	// Должен считать то же, что Series (rateSeries) — иначе алерт и график разойдутся;
	// increase — единственное исключение, порог берёт прирост за окно, не скорость.
	if typ == "sum" && monotonic && temporality == "cumulative" {
		if agg == "increase" {
			return q.aggregateIncrease(ctx, projectID, name, environment, host, matchers, from, to)
		}
		return q.aggregateRate(ctx, projectID, name, environment, host, matchers, agg, from, to)
	}
	base := fmt.Sprintf(`FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  AND (? = '' OR host = ?) %s`, matchersClause(matchers))
	args := []any{projectID, name, from, to, environment, environment, host, host}
	args = appendMatchersArgs(args, matchers)

	if typ == "histogram" && isPercentile(agg) {
		row := q.conn.QueryRow(ctx, "SELECT sumForEach(bucket_counts), any(explicit_bounds), sum(count) "+base, args...)
		var bc []uint64
		var eb []float64
		var cnt uint64
		if err := row.Scan(&bc, &eb, &cnt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, false, nil // пустое окно при empty_result_for_aggregation_by_empty_set=1
			}
			return 0, false, fmt.Errorf("metric: aggregate histogram: %w", err)
		}
		if cnt == 0 {
			return 0, false, nil
		}
		return histogramQuantile(bc, eb, percentileValue(agg)), true, nil
	}

	row := q.conn.QueryRow(ctx, fmt.Sprintf("SELECT %s, count() %s", scalarAggExpr(typ, agg), base), args...)
	var v float64
	var cnt uint64
	if err := row.Scan(&v, &cnt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil // пустое окно при empty_result_for_aggregation_by_empty_set=1
		}
		return 0, false, fmt.Errorf("metric: aggregate: %w", err)
	}
	if cnt == 0 {
		return 0, false, nil
	}
	return v, true, nil
}

// Порядка величины графика (metricChartBuckets=120): агрегат должен считаться
// по тем же величинам, что видит человек.
const aggregateRateBuckets = 60

// Тем же способом, что Series — алерт и график остаются согласованы.
func (q *Query) aggregateRate(ctx context.Context, projectID int64, name, environment, host string, matchers []LabelMatcher, agg string, from, to time.Time) (float64, bool, error) {
	stepSec := int64(to.Sub(from).Seconds()) / aggregateRateBuckets
	if stepSec < 1 {
		stepSec = 1
	}
	pts, err := q.rateSeries(ctx, projectID, name, environment, host, matchers, from, to, stepSec)
	if err != nil {
		return 0, false, err
	}
	if len(pts) == 0 {
		return 0, false, nil
	}
	return aggregatePoints(pts, agg), true, nil
}

// Перцентили — честный ближайший ранг по отсортированным значениям, не среднее,
// как scalarAggExpr для не-гистограмм.
func aggregatePoints(pts []Point, agg string) float64 {
	vals := make([]float64, len(pts))
	for i, p := range pts {
		vals[i] = p.V
	}
	switch agg {
	case "max":
		out := vals[0]
		for _, v := range vals[1:] {
			if v > out {
				out = v
			}
		}
		return out
	case "min":
		out := vals[0]
		for _, v := range vals[1:] {
			if v < out {
				out = v
			}
		}
		return out
	case "sum":
		var out float64
		for _, v := range vals {
			out += v
		}
		return out
	}
	if isPercentile(agg) {
		sort.Float64s(vals)
		// ceil(p*n)-1: округление вверх осознанно — floor занижал бы перцентиль,
		// а для алерта пропущенное превышение хуже лишнего срабатывания.
		idx := int(math.Ceil(percentileValue(agg)*float64(len(vals)))) - 1
		if idx < 0 {
			idx = 0
		}
		if idx >= len(vals) {
			idx = len(vals) - 1
		}
		return vals[idx]
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func (q *Query) scanPoints(ctx context.Context, sql string, args []any) ([]Point, error) {
	rows, err := q.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("metric: series: %w", err)
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.T, &p.V); err != nil {
			return nil, fmt.Errorf("metric: series scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scalarAggExpr(typ, agg string) string {
	if typ == "histogram" {
		return "if(sum(count) = 0, 0, sum(value) / sum(count))"
	}
	switch agg {
	case "max":
		return "max(value)"
	case "min":
		return "min(value)"
	case "sum", "increase":
		// increase на не-кумулятивный sum: значения уже точечные приращения,
		// сумма по окну и есть честный прирост — как aggregateIncrease для cumulative.
		return "sum(value)"
	case "last":
		return "argMax(value, ts)"
	}
	if isPercentile(agg) {
		return fmt.Sprintf("quantile(%g)(value)", percentileValue(agg))
	}
	return "avg(value)"
}

func compactMatchers(ms []LabelMatcher) []LabelMatcher {
	out := ms[:0:0]
	for _, m := range ms {
		if m.Key != "" {
			out = append(out, m)
		}
	}
	return out
}

// Вызывающий обязан заранее прогнать ms через compactMatchers.
func matchersClause(ms []LabelMatcher) string {
	var b strings.Builder
	for range ms {
		b.WriteString(" AND attributes[?] = ?")
	}
	return b.String()
}

func appendMatchersArgs(args []any, ms []LabelMatcher) []any {
	for _, m := range ms {
		args = append(args, m.Key, m.Value)
	}
	return args
}

func isPercentile(agg string) bool {
	return agg == "p50" || agg == "p95" || agg == "p99"
}

func percentileValue(agg string) float64 {
	switch agg {
	case "p50":
		return 0.5
	case "p95":
		return 0.95
	case "p99":
		return 0.99
	default:
		return 0.5
	}
}

// Нижняя граница первого бакета — min(0, bounds[0]): при отрицательных bounds
// зашитый ноль оказался бы выше верхней границы, и квантиль вышел бы за неё.
func histogramQuantile(bucketCounts []uint64, bounds []float64, q float64) float64 {
	var total uint64
	for _, c := range bucketCounts {
		total += c
	}
	if total == 0 || len(bucketCounts) == 0 {
		return 0
	}
	target := q * float64(total)
	var cum float64
	for i, c := range bucketCounts {
		prevCum := cum
		cum += float64(c)
		if cum >= target {
			lower := 0.0
			if i > 0 && i-1 < len(bounds) {
				lower = bounds[i-1]
			} else if i == 0 && len(bounds) > 0 {
				lower = math.Min(0, bounds[0])
			}
			if i >= len(bounds) { // последний бесконечный бакет
				return lower
			}
			upper := bounds[i]
			if c == 0 {
				return lower
			}
			frac := (target - prevCum) / float64(c)
			return lower + frac*(upper-lower)
		}
	}
	if len(bounds) > 0 {
		return bounds[len(bounds)-1]
	}
	return 0
}
