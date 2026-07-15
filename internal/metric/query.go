package metric

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Query читает агрегаты метрик из metric_points (аналог trace.Query).
type Query struct {
	conn driver.Conn
}

func NewQuery(conn driver.Conn) *Query {
	return &Query{conn: conn}
}

// MetricInfo — метрика в перечне проекта (для страницы списка).
type MetricInfo struct {
	Name string
	Type string
	Unit string
}

// Point — точка временного ряда.
type Point struct {
	T time.Time
	V float64
}

// LabelMatcher — фильтр по одному лейблу (пустой Key → без фильтра).
type LabelMatcher struct {
	Key   string
	Value string
}

// ListMetrics возвращает уникальные метрики проекта (имя/тип/юнит), с
// опциональным фильтром по environment.
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

// Labels возвращает ключи→значения лейблов метрики (до 20 значений на ключ) для
// фильтров UI.
func (q *Query) Labels(ctx context.Context, projectID int64, name string) (map[string][]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT k, groupUniqArray(20)(v) FROM (
			SELECT
				arrayJoin(mapKeys(attributes)) AS k,
				attributes[k] AS v
			FROM metric_points
			WHERE project_id = ? AND name = ?
		)
		GROUP BY k
		ORDER BY k`,
		projectID, name)
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

// Environments возвращает известные окружения метрики (для фильтра).
func (q *Query) Environments(ctx context.Context, projectID int64, name string) ([]string, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT DISTINCT environment FROM metric_points
		WHERE project_id = ? AND name = ? AND environment != ''
		ORDER BY environment`,
		projectID, name)
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

// metricType возвращает тип метрики (gauge/sum/histogram) и признак monotonic —
// нужно, чтобы Series выбрал стратегию агрегации.
func (q *Query) metricType(ctx context.Context, projectID int64, name string) (typ string, monotonic bool, temporality string, err error) {
	row := q.conn.QueryRow(ctx, `
		SELECT any(type), any(monotonic), any(temporality)
		FROM metric_points WHERE project_id = ? AND name = ?`,
		projectID, name)
	var mono uint8
	if err := row.Scan(&typ, &mono, &temporality); err != nil {
		return "", false, "", fmt.Errorf("metric: metric type: %w", err)
	}
	return typ, mono == 1, temporality, nil
}

// Series возвращает временной ряд метрики: bucketing по step, агрегация по типу
// (gauge/sum non-monotonic → agg; sum monotonic cumulative → rate; histogram +
// p50/p95/p99 → перцентиль интерполяцией, histogram + avg → sum/count).
func (q *Query) Series(ctx context.Context, projectID int64, name, environment string, matcher LabelMatcher, agg string, from, to time.Time, step time.Duration) ([]Point, error) {
	typ, monotonic, temporality, err := q.metricType(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	stepSec := int64(step.Seconds())
	if stepSec < 1 {
		stepSec = 1
	}

	switch {
	case typ == "histogram" && isPercentile(agg):
		return q.histogramSeries(ctx, projectID, name, environment, matcher, agg, from, to, stepSec)
	case typ == "sum" && monotonic && temporality == "cumulative":
		return q.rateSeries(ctx, projectID, name, environment, matcher, from, to, stepSec)
	default:
		return q.scalarSeries(ctx, projectID, name, environment, matcher, typ, agg, from, to, stepSec)
	}
}

// scalarSeries — простая агрегация значения по бакету. Для histogram+avg
// используем sum(value)/sum(count) (среднее наблюдение).
func (q *Query) scalarSeries(ctx context.Context, projectID int64, name, environment string, matcher LabelMatcher, typ, agg string, from, to time.Time, stepSec int64) ([]Point, error) {
	aggExpr := scalarAggExpr(typ, agg)
	sql := fmt.Sprintf(`
		SELECT toStartOfInterval(ts, INTERVAL %d second) AS b, %s
		FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  %s
		GROUP BY b ORDER BY b`, stepSec, aggExpr, matcherClause(matcher))
	args := []any{projectID, name, from, to, environment, environment}
	args = appendMatcherArgs(args, matcher)
	return q.scanPoints(ctx, sql, args)
}

// rateSeries — rate для monotonic cumulative counter'а: max(value) по бакету,
// затем разность соседних бакетов / шаг. Сброс счётчика (отрицательная
// разность) → 0.
func (q *Query) rateSeries(ctx context.Context, projectID int64, name, environment string, matcher LabelMatcher, from, to time.Time, stepSec int64) ([]Point, error) {
	sql := fmt.Sprintf(`
		SELECT toStartOfInterval(ts, INTERVAL %d second) AS b, max(value)
		FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  %s
		GROUP BY b ORDER BY b`, stepSec, matcherClause(matcher))
	args := []any{projectID, name, from, to, environment, environment}
	args = appendMatcherArgs(args, matcher)
	cum, err := q.scanPoints(ctx, sql, args)
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
		out = append(out, Point{T: cum[i].T, V: delta / float64(stepSec)})
	}
	return out, nil
}

// histogramSeries — перцентиль по бакету: суммируем bucket_counts точек бакета,
// берём any(explicit_bounds) и считаем квантиль в Go.
func (q *Query) histogramSeries(ctx context.Context, projectID int64, name, environment string, matcher LabelMatcher, agg string, from, to time.Time, stepSec int64) ([]Point, error) {
	sql := fmt.Sprintf(`
		SELECT toStartOfInterval(ts, INTERVAL %d second) AS b,
		       sumForEach(bucket_counts) AS bc,
		       any(explicit_bounds) AS eb
		FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?)
		  %s
		GROUP BY b ORDER BY b`, stepSec, matcherClause(matcher))
	args := []any{projectID, name, from, to, environment, environment}
	args = appendMatcherArgs(args, matcher)
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

// Aggregate возвращает единственное значение агрегата метрики за всё окно
// [from,to) (без бакетинга) и признак наличия данных. Для histogram+перцентиль
// суммирует bucket_counts всего окна и считает квантиль. Используется оценщиком
// пороговых алертов: «avg метрики за окно ⋛ порог». ok=false — данных нет.
func (q *Query) Aggregate(ctx context.Context, projectID int64, name, environment string, matcher LabelMatcher, agg string, from, to time.Time) (float64, bool, error) {
	typ, _, _, err := q.metricType(ctx, projectID, name)
	if err != nil {
		return 0, false, err
	}
	base := fmt.Sprintf(`FROM metric_points
		WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ?
		  AND (? = '' OR environment = ?) %s`, matcherClause(matcher))
	args := []any{projectID, name, from, to, environment, environment}
	args = appendMatcherArgs(args, matcher)

	if typ == "histogram" && isPercentile(agg) {
		row := q.conn.QueryRow(ctx, "SELECT sumForEach(bucket_counts), any(explicit_bounds), sum(count) "+base, args...)
		var bc []uint64
		var eb []float64
		var cnt uint64
		if err := row.Scan(&bc, &eb, &cnt); err != nil {
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
		return 0, false, fmt.Errorf("metric: aggregate: %w", err)
	}
	if cnt == 0 {
		return 0, false, nil
	}
	return v, true, nil
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

// scalarAggExpr — CH-выражение агрегации для скалярных метрик.
func scalarAggExpr(typ, agg string) string {
	if typ == "histogram" {
		// histogram + не-перцентиль → среднее наблюдение.
		return "if(sum(count) = 0, 0, sum(value) / sum(count))"
	}
	switch agg {
	case "max":
		return "max(value)"
	case "min":
		return "min(value)"
	case "sum":
		return "sum(value)"
	case "last":
		return "argMax(value, ts)"
	default: // avg
		return "avg(value)"
	}
}

func matcherClause(m LabelMatcher) string {
	if m.Key == "" {
		return ""
	}
	return "AND attributes[?] = ?"
}

func appendMatcherArgs(args []any, m LabelMatcher) []any {
	if m.Key == "" {
		return args
	}
	return append(args, m.Key, m.Value)
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

// histogramQuantile оценивает квантиль q (0..1) по счётчикам бакетов гистограммы
// с верхними границами bounds (bucketCounts на 1 длиннее bounds — последний
// бакет (bounds[last], +inf)). Линейная интерполяция внутри бакета; для
// последнего (бесконечного) бакета возвращаем его нижнюю границу как суррогат.
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
