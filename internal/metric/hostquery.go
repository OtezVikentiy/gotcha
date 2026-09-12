package metric

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Хост, приславший метрики проекта, и время его последней точки.
type HostActivity struct {
	Host   string
	LastTS time.Time
}

// Хосты проекта, активные в окне [from,to), с временем последней точки каждого — пустой host (метрики
// без host-атрибуции) исключён.
func (q *Query) Hosts(ctx context.Context, projectID int64, from, to time.Time) ([]HostActivity, error) {
	rows, err := q.conn.Query(ctx, `
		SELECT host, max(ts) FROM metric_points
		WHERE project_id = ? AND host != '' AND ts >= ? AND ts < ?
		GROUP BY host ORDER BY host`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("metric: hosts: %w", err)
	}
	defer rows.Close()
	var out []HostActivity
	for rows.Next() {
		var h HostActivity
		if err := rows.Scan(&h.Host, &h.LastTS); err != nil {
			return nil, fmt.Errorf("metric: hosts scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Двухуровневый агрегат: хост-метрики мульти-лейбловые ВНУТРИ хоста — одноуровневый argMax по host
// взял бы ОДИН случайный под-лейбл; внутренний argMax берёт каждую под-серию, внешний max/avg сворачивает их.
func (q *Query) LatestByHost(ctx context.Context, projectID int64, name string,
	matchers []LabelMatcher, subKey, subAgg string, from, to time.Time) (map[string]float64, error) {

	outer := "max(v)"
	if subAgg == "avg" {
		outer = "avg(v)"
	}
	ms := compactMatchers(matchers)
	var sqlText string
	args := []any{projectID, name, from, to}
	if subKey == "" {
		sqlText = fmt.Sprintf(`
			SELECT host, argMax(value, ts) AS v FROM metric_points
			WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ? AND host != ''%s
			GROUP BY host`, matchersClause(ms))
		args = appendMatchersArgs(args, ms)
	} else {
		sqlText = fmt.Sprintf(`
			SELECT host, %s FROM (
				SELECT host, attributes[?] AS sk, argMax(value, ts) AS v
				FROM metric_points
				WHERE project_id = ? AND name = ? AND ts >= ? AND ts < ? AND host != ''%s
				GROUP BY host, sk
			) GROUP BY host`, outer, matchersClause(ms))
		args = append([]any{subKey}, appendMatchersArgs(args, ms)...)
	}

	rows, err := q.conn.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("metric: latest by host: %w", err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var host string
		var v float64
		if err := rows.Scan(&host, &v); err != nil {
			return nil, fmt.Errorf("metric: latest by host scan: %w", err)
		}
		out[host] = v
	}
	return out, rows.Err()
}

// Временной ряд одной группы (значения атрибута groupKey) для мульти-линейного графика карточки хоста.
type GroupedSeries struct {
	Key    string
	Points []Point
}

// Набор рядов по группам, отсортированный по убыванию среднего, с усечением до MaxSeriesGroups.
type GroupedSeriesResult struct {
	Groups    []GroupedSeries
	Truncated bool
}

// Без него хост с сотней mountpoint/cpu/device превратил бы график в кашу. Экспортирована — подпись
// усечения на карточке обязана называть ЭТО число, не своё захардкоженное, иначе они разъедутся.
const MaxSeriesGroups = 8

// Скалярная агрегация agg по бакету ВНУТРИ каждой группы (например, mountpoint или cpu). Групп не больше
// MaxSeriesGroups — топ по среднему, порядок по убыванию среднего (стабильный для легенды).
func (q *Query) SeriesGrouped(ctx context.Context, projectID int64, name, host, groupKey, agg string, from, to time.Time, step time.Duration) (GroupedSeriesResult, error) {
	// Клэмп самого step, не только его секундного слепка для SQL — иначе step=0 утёк бы в Go-арифметику
	// при будущих правках (см. SeriesGroupedRate ниже).
	if step < time.Second {
		step = time.Second
	}
	stepSec := int64(step.Seconds())
	typ, _, _, err := q.metricType(ctx, projectID, name, from, to)
	if err != nil {
		return GroupedSeriesResult{}, err
	}
	// Пустой-байпас host — симметрия с Series: host=="" значит «все хосты», не «буквально пустой host».
	// Нужно рецептам сервисов — их метрики без resourcedetection, host у точек пуст.
	sqlText := fmt.Sprintf(`
		SELECT attributes[?] AS g, toStartOfInterval(ts, INTERVAL %d second) AS b, %s
		FROM metric_points
		WHERE project_id = ? AND name = ? AND (? = '' OR host = ?) AND ts >= ? AND ts < ?
		GROUP BY g, b ORDER BY g, b`, stepSec, scalarAggExpr(typ, agg))
	rows, err := q.conn.Query(ctx, sqlText, groupKey, projectID, name, host, host, from, to)
	if err != nil {
		return GroupedSeriesResult{}, fmt.Errorf("metric: series grouped: %w", err)
	}
	defer rows.Close()

	var order []string
	byKey := map[string][]Point{}
	for rows.Next() {
		var g string
		var p Point
		if err := rows.Scan(&g, &p.T, &p.V); err != nil {
			return GroupedSeriesResult{}, fmt.Errorf("metric: series grouped scan: %w", err)
		}
		if _, ok := byKey[g]; !ok {
			order = append(order, g)
		}
		byKey[g] = append(byKey[g], p)
	}
	if err := rows.Err(); err != nil {
		return GroupedSeriesResult{}, err
	}
	groups := make([]GroupedSeries, 0, len(order))
	for _, k := range order {
		groups = append(groups, GroupedSeries{Key: k, Points: byKey[k]})
	}
	return topNSeriesGroups(groups), nil
}

// Rate считается на мелкой размерности (groupKey, deviceKey) — дельта СВОЕГО устройства между соседними
// бакетами, отрицательная → 0; суммируется по groupKey ПОСЛЕ. Иначе дельта смешала бы разные счётчики.
func (q *Query) SeriesGroupedRate(ctx context.Context, projectID int64, name, host, groupKey, deviceKey string, from, to time.Time, step time.Duration) (GroupedSeriesResult, error) {
	// Клэмп самого step: он участвует в Go-арифметике размазывания ниже (n := gap/step) — некэмпленный
	// step=0 паникует, суб-секундный step рассинхронил бы сетки SQL и Go.
	if step < time.Second {
		step = time.Second
	}
	stepSec := int64(step.Seconds())
	// Пустой-байпас host — та же симметрия с Series, что в SeriesGrouped выше: host=="" = «все хосты».
	sqlText := fmt.Sprintf(`
		SELECT attributes[?] AS g, attributes[?] AS d,
		       toStartOfInterval(ts, INTERVAL %d second) AS b, max(value) AS v
		FROM metric_points
		WHERE project_id = ? AND name = ? AND (? = '' OR host = ?) AND ts >= ? AND ts < ?
		GROUP BY g, d, b ORDER BY g, d, b`, stepSec)
	rows, err := q.conn.Query(ctx, sqlText, groupKey, deviceKey, projectID, name, host, host, from, to)
	if err != nil {
		return GroupedSeriesResult{}, fmt.Errorf("metric: series grouped rate: %w", err)
	}
	defer rows.Close()

	// Строки идут упорядоченными по (g, d, b) — копим кумулятив ТЕКУЩЕГО
	// устройства, и при смене (g, d) сбрасываем накопленный ряд в devicesByGroup.
	var order []string
	devicesByGroup := map[string][][]Point{}
	var curG, curD string
	var curPts []Point
	started := false
	flush := func() {
		if curPts == nil {
			return
		}
		if _, ok := devicesByGroup[curG]; !ok {
			order = append(order, curG)
		}
		devicesByGroup[curG] = append(devicesByGroup[curG], curPts)
	}
	for rows.Next() {
		var g, d string
		var p Point
		if err := rows.Scan(&g, &d, &p.T, &p.V); err != nil {
			return GroupedSeriesResult{}, fmt.Errorf("metric: series grouped rate scan: %w", err)
		}
		if !started || g != curG || d != curD {
			flush()
			curG, curD, curPts = g, d, nil
			started = true
		}
		curPts = append(curPts, p)
	}
	flush()
	if err := rows.Err(); err != nil {
		return GroupedSeriesResult{}, err
	}

	groups := make([]GroupedSeries, 0, len(order))
	for _, g := range order {
		acc := map[time.Time]float64{}
		for _, devPts := range devicesByGroup[g] {
			addDeviceRateContribution(acc, devPts, step)
		}
		if len(acc) == 0 {
			continue
		}
		pts := make([]Point, 0, len(acc))
		for t, v := range acc {
			pts = append(pts, Point{T: t, V: v})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].T.Before(pts[j].T) })
		groups = append(groups, GroupedSeries{Key: g, Points: pts})
	}
	return topNSeriesGroups(groups), nil
}

// Rate одного устройства по его кумулятивным точкам (max(value) по бакету, отсортированы по ts) —
// та же арифметика, что rateSeries (query.go), на размерности одного устройства вместо всей серии.
func addDeviceRateContribution(acc map[time.Time]float64, pts []Point, step time.Duration) {
	if len(pts) < 2 {
		return
	}
	// Защитный клэмп: step участвует в целочисленном делении (n := gap/step) ниже — второй рубеж на случай
	// вызова в обход клэмпа в SeriesGroupedRate.
	if step < time.Second {
		step = time.Second
	}
	stepSec := step.Seconds()
	for i := 1; i < len(pts); i++ {
		delta := pts[i].V - pts[i-1].V
		if delta < 0 {
			delta = 0
		}
		// Делим на РЕАЛЬНЫЙ интервал между точками устройства, не на ширину бакета — GROUP BY отдаёт только
		// непустые бакеты; деление на step завысило бы скорость при скрейпе реже шага.
		gap := pts[i].T.Sub(pts[i-1].T)
		gapSec := gap.Seconds()
		if gapSec <= 0 {
			gapSec = stepSec
			gap = step
		}
		rate := delta / gapSec

		// «Размазывание»: если устройство скрейпится реже шага (n>1 шагов между A и B), у него нет точки в
		// промежуточных бакетах — вклад rate относится на КАЖДЫЙ из n бакетов (A,B], иначе группа проседала бы ложно.
		n := int64(gap / step)
		if n < 1 {
			n = 1
		}
		for m := int64(1); m <= n; m++ {
			acc[pts[i-1].T.Add(time.Duration(m)*step)] += rate
		}
	}
}

func topNSeriesGroups(groups []GroupedSeries) GroupedSeriesResult {
	// SliceStable — при равных средних порядок групп не должен «прыгать» между
	// вызовами (стабильная легенда графика).
	sort.SliceStable(groups, func(i, j int) bool {
		return avgPointsValue(groups[i].Points) > avgPointsValue(groups[j].Points)
	})
	truncated := len(groups) > MaxSeriesGroups
	if truncated {
		groups = groups[:MaxSeriesGroups]
	}
	return GroupedSeriesResult{Groups: groups, Truncated: truncated}
}

func avgPointsValue(pts []Point) float64 {
	if len(pts) == 0 {
		return 0
	}
	var sum float64
	for _, p := range pts {
		sum += p.V
	}
	return sum / float64(len(pts))
}
