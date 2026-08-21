package metric

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// HostActivity — хост, приславший метрики проекта, и время его последней точки.
type HostActivity struct {
	Host   string
	LastTS time.Time
}

// Hosts возвращает хосты проекта, активные в окне [from,to), с временем
// последней точки каждого (для страницы списка хостов). Пустой host (метрики
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

// LatestByHost — «последние значения» метрики по каждому хосту (для карточек
// списка хостов). Двухуровневый агрегат: хост-метрики (диск, CPU и т.п.)
// мульти-лейбловые ВНУТРИ хоста — на хосте одновременно живут точки на
// mountpoint="/", mountpoint="/var", cpu="0", cpu="1" и т.д. Одноуровневый
// argMax(value, ts) по одному host взял бы значение ОДНОГО случайного
// под-лейбла (какой из них выиграет — определяется порядком строк внутри
// ClickHouse, не значением). Поэтому сначала внутренний argMax(value, ts) по
// (host, attributes[subKey]) берёт последнее значение КАЖДОЙ под-серии
// (каждого mountpoint / ядра), и только затем внешний max/avg сворачивает
// набор под-серий хоста в одно число. subKey=="" — метрика без под-лейбла
// (одна серия на хост), тогда двухуровневая свёртка не нужна и используется
// одноуровневый argMax по host напрямую.
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

// GroupedSeries — временной ряд одной группы (одного значения атрибута
// groupKey) для мульти-линейного графика карточки хоста.
type GroupedSeries struct {
	Key    string
	Points []Point
}

// GroupedSeriesResult — набор рядов по группам, отсортированный по убыванию
// среднего значения, с усечением до MaxSeriesGroups.
type GroupedSeriesResult struct {
	Groups    []GroupedSeries
	Truncated bool
}

// MaxSeriesGroups — потолок числа линий на графике карточки хоста. Без него
// хост с сотней mountpoint/cpu/device превратил бы график в нечитаемую кашу.
//
// Экспортирована, потому что подпись усечения на карточке («показаны топ-8
// групп») обязана называть ЭТО число, а не своё: до правки «8» было вписано в
// перевод обеих локалей и при смене потолка молча разъехалось бы с
// действительностью (UX-аудит A1, P2-2).
const MaxSeriesGroups = 8

// SeriesGrouped возвращает временной ряд метрики хоста, разбитый по значению
// атрибута groupKey (например, mountpoint или cpu) — скалярная агрегация agg
// по бакету ВНУТРИ каждой группы. Групп оставляют не больше MaxSeriesGroups —
// топ по среднему значению; порядок групп по убыванию среднего (стабильный
// для легенды графика).
func (q *Query) SeriesGrouped(ctx context.Context, projectID int64, name, host, groupKey, agg string, from, to time.Time, step time.Duration) (GroupedSeriesResult, error) {
	// Клэмп самого step (не только его секундного слепка для SQL) — step=0
	// иначе утекал бы в Go-арифметику при будущих правках этой функции тем же
	// путём, что уронил SeriesGroupedRate (см. её комментарий ниже).
	if step < time.Second {
		step = time.Second
	}
	stepSec := int64(step.Seconds())
	typ, _, _, err := q.metricType(ctx, projectID, name, from, to)
	if err != nil {
		return GroupedSeriesResult{}, err
	}
	// Пустой-байпас host — симметрия с Series (scalarSeries, query.go): host==""
	// означает «все хосты», а не «строки с буквально пустым host». Нужно
	// рецептам сервисов B6 — их метрики приходят без resourcedetection, host у
	// точек пуст, и жёсткий `host = ?` оставлял бы график рецепта пустым.
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

// SeriesGroupedRate — rate-версия SeriesGrouped для monotonic cumulative
// счётчиков хоста (например, system.network.io по device), где groupKey —
// «крупный» атрибут легенды (direction), а deviceKey — «мелкий» атрибут
// физического источника счётчика (device). Rate обязан считаться на мелкой
// размерности (groupKey, deviceKey) — дельта СВОЕГО устройства между его
// соседними бакетами, отрицательная → 0 (сброс счётчика), — и только ПОТОМ
// скорости суммируются по groupKey. Если бы rate считался после max(value) по
// (groupKey, bucket) без разбивки по device, дельта между бакетами со смесью
// счётчиков разных устройств была бы бессмысленной (см. TestSeriesGroupedRateSumsDevices).
func (q *Query) SeriesGroupedRate(ctx context.Context, projectID int64, name, host, groupKey, deviceKey string, from, to time.Time, step time.Duration) (GroupedSeriesResult, error) {
	// Клэмп самого step, а не только его секундного слепка для SQL: step
	// напрямую участвует в Go-арифметике размазывания ниже (n := gap/step,
	// Add(m*step)) — некэмпленный step=0 там даёт панику integer divide by
	// zero, а суб-секундный step рассинхронил бы сетку бакетов SQL (округлённую
	// до stepSec>=1) с сеткой размазывания в Go.
	if step < time.Second {
		step = time.Second
	}
	stepSec := int64(step.Seconds())
	// Пустой-байпас host — та же симметрия с Series, что и в SeriesGrouped выше
	// (см. комментарий там): host=="" = «все хосты», нужно рецептам B6.
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

// addDeviceRateContribution считает rate ОДНОГО устройства (device) по его
// кумулятивным точкам pts (уже max(value) по бакету, отсортированы по ts) и
// добавляет вклад в acc — карту «сеточный бакет → сумма скоростей». Логика
// дельты/сброса/деления на реальный зазор скопирована из rateSeries
// (query.go) — та же арифметика rate для monotonic cumulative счётчика,
// применённая на мелкой размерности одного устройства вместо всей серии.
func addDeviceRateContribution(acc map[time.Time]float64, pts []Point, step time.Duration) {
	if len(pts) < 2 {
		return
	}
	// Защитный клэмп: step участвует в целочисленном делении (n := gap / step)
	// ниже, некэмпленный step=0 паникует. Вызывающий (SeriesGroupedRate) уже
	// клэмпит step перед вызовом — это второй рубеж на случай будущего вызова
	// в обход него.
	if step < time.Second {
		step = time.Second
	}
	stepSec := step.Seconds()
	for i := 1; i < len(pts); i++ {
		delta := pts[i].V - pts[i-1].V
		if delta < 0 {
			delta = 0
		}
		// Делим на РЕАЛЬНЫЙ интервал между соседними точками УСТРОЙСТВА, а не на
		// ширину бакета: GROUP BY возвращает только НЕПУСТЫЕ бакеты, поэтому при
		// скрейпе реже шага соседние точки устройства отстоят на несколько шагов.
		// Деление на step завысило бы скорость ровно в (интервал/step) раз —
		// скрейп раз в 300с при шаге 60с дал бы 5.0/с вместо 1.0/с.
		gap := pts[i].T.Sub(pts[i-1].T)
		gapSec := gap.Seconds()
		if gapSec <= 0 {
			gapSec = stepSec
			gap = step
		}
		rate := delta / gapSec

		// «Размазывание»: A и B — соседние бакеты устройства, оба выровнены по
		// шагу сетки, поэтому (B-A) — целое число шагов n. Если устройство
		// скрейпится реже шага (n>1), у него физически НЕТ точки в промежуточных
		// сеточных бакетах — но трафик шёл всё это время, и его скорость известна
		// только как средняя по интервалу (A,B]. Поэтому вклад rate относится на
		// КАЖДЫЙ из n сеточных бакетов интервала (A,B], а не только на бакет B.
		// Иначе сумма по группе проседала бы ложными провалами именно там, где у
		// ЭТОГО устройства нет собственной точки, хотя другие устройства группы
		// продолжают сообщать данные (TestSeriesGroupedRateSparseDevice).
		n := int64(gap / step)
		if n < 1 {
			n = 1
		}
		for m := int64(1); m <= n; m++ {
			acc[pts[i-1].T.Add(time.Duration(m)*step)] += rate
		}
	}
}

// topNSeriesGroups сортирует группы по убыванию среднего значения (для
// стабильной легенды графика) и оставляет не больше MaxSeriesGroups.
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
