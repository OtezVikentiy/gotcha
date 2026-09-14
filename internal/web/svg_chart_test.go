package web

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

func TestMetricSeriesMarkupAxes(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	points := []metric.Point{
		{T: base, V: 10},
		{T: base.Add(30 * time.Minute), V: 40},
		{T: base.Add(time.Hour), V: 25},
	}
	thresholds := []metricThreshold{{Value: 30, Comparator: "gt"}}
	out := metricSeriesMarkup(context.Background(), points, "ms", thresholds, nil, 720, 200)

	for _, want := range []string{
		`class="metric-chart chart-vb720"`,
		`class="chart-axis"`,
		`class="chart-threshold"`,
		`stroke-dasharray`,
		`<polyline`,
		"10:00",
		"ms",
		"&gt; 30 ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metric chart markup missing %q\n%s", want, out)
		}
	}
}

func TestMetricSeriesMarkupEmpty(t *testing.T) {
	out := metricSeriesMarkup(context.Background(), nil, "", nil, nil, 720, 200)
	if !strings.Contains(out, "chart-axis") {
		t.Errorf("empty metric chart should still draw axes: %s", out)
	}
	if !strings.Contains(out, "нет данных") {
		t.Errorf("empty metric chart should note absence of data: %s", out)
	}
}

func TestMetricSeriesMarkupThresholdInDomain(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	points := []metric.Point{{T: base, V: 10}, {T: base.Add(time.Hour), V: 20}}
	out := metricSeriesMarkup(context.Background(), points, "", []metricThreshold{{Value: 15, Comparator: "lt"}}, nil, 720, 200)
	if !strings.Contains(out, "chart-threshold") {
		t.Errorf("threshold within data range must be drawn: %s", out)
	}
	if !strings.Contains(out, "&lt; 15") {
		t.Errorf("lt threshold label должен использовать знак <: %s", out)
	}
}

// ±Inf, не только NaN, обязаны фильтроваться из домена — иначе domMax-domMin
// становится бесконечным и NaN-координаты получают все точки, не только сбойная.
func TestMetricSeriesMarkupIgnoresInf(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	points := []metric.Point{
		{T: base, V: 10},
		{T: base.Add(30 * time.Minute), V: math.Inf(1)},
		{T: base.Add(time.Hour), V: 25},
	}
	out := metricSeriesMarkup(context.Background(), points, "ms", nil, nil, 720, 200)
	if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
		t.Errorf("Inf point must not leak into coordinates: %s", out)
	}
	if !strings.Contains(out, "10 ms") || !strings.Contains(out, "25 ms") {
		t.Errorf("finite domain from non-Inf points expected: %s", out)
	}
}

// плоский ряд (dataMin==dataMax) должен рисовать одну Y-подпись, не три
// наложенных (max/середина/min с одинаковым значением).
func TestMetricSeriesMarkupFlatSeriesSingleYLabel(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	points := []metric.Point{{T: base, V: 5}, {T: base.Add(time.Hour), V: 5}}
	thresholds := []metricThreshold{{Value: 20, Comparator: "gt"}}
	out := metricSeriesMarkup(context.Background(), points, "ms", thresholds, nil, 720, 200)

	// этот атрибутный набор — только у подписей оси Y, не у hover-band/порога
	// с тем же текстом.
	if got := strings.Count(out, `dominant-baseline="middle" fill="currentColor">5 ms</text>`); got != 1 {
		t.Errorf("flat series must draw a single Y-axis label, got %d: %s", got, out)
	}
}

func TestChartBarsAxes(t *testing.T) {
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	points := []event.Point{
		{T: base, N: 3},
		{T: base.Add(72 * time.Hour), N: 7},
		{T: base.Add(144 * time.Hour), N: 1},
	}
	out := chartBars(context.Background(), points, chartWidth, chartHeight)
	for _, want := range []string{
		`class="chart-freq chart-vb1200"`,
		`class="chart-axis"`,
		`<rect`,
		">0<",
		">10<", // верх шкалы: на шаг выше максимума (max=7, шаг 5)
		">5<",
		"18.07",
		"21.07", // метки дней ставятся на каждой границе суток
		"<title>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frequency chart markup missing %q\n%s", want, out)
		}
	}
}

func TestChartBarsTooltip(t *testing.T) {
	base := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	out := chartBars(context.Background(), []event.Point{{T: base, N: 5}}, chartWidth, chartHeight)

	if strings.Count(out, "<title>") != 1 {
		t.Errorf("ожидалась одна подсказка на один столбик: %s", out)
	}
	// время — через humanize.Time (дата, время, пояс).
	for _, want := range []string{"2026-07-18 15:00 UTC", "5 событий"} {
		if !strings.Contains(out, want) {
			t.Errorf("подсказка без %q: %s", want, out)
		}
	}
}

// шаг сетки — из ряда 1/2/5×10ⁿ, иначе подписи вида 37/74/111 не дают
// прикинуть значение.
func TestNiceStep(t *testing.T) {
	cases := []struct {
		max  uint64
		want uint64
	}{
		{1, 1},
		{3, 1},
		{7, 5},
		{30, 10},
		{111, 50},
		{0, 1},
	}
	for _, c := range cases {
		if got := niceStep(c.max, 3); got != c.want {
			t.Errorf("niceStep(%d, 3) = %d, want %d", c.max, got, c.want)
		}
	}
}

func TestChartBarsEmpty(t *testing.T) {
	out := chartBars(context.Background(), nil, chartWidth, chartHeight)
	if !strings.Contains(out, "chart-axis") {
		t.Errorf("empty frequency chart should draw axes: %s", out)
	}
}

// верх шкалы строго выше максимума — иначе столбик упирается в рамку и
// график выглядит сплошным забором.
func TestChartBarsHeadroom(t *testing.T) {
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		max     uint64
		wantTop string
	}{
		{10, ">15<"}, // шаг 5, ближайшее кратное равно максимуму → +шаг
		{1, ">2<"},   // шаг 1
		{3, ">4<"},   // шаг 1
	}
	for _, c := range cases {
		out := chartBars(context.Background(), []event.Point{{T: base, N: c.max}}, chartWidth, chartHeight)
		if !strings.Contains(out, c.wantTop) {
			t.Errorf("max=%d: верх шкалы не %s\n%s", c.max, c.wantTop, out)
		}
	}
}

// ряд реже сетки корзин (раз в час при 12-минутном шаге) должен остаться
// одной линией, не рассыпаться на изолированные сегменты.
func TestMetricSeriesSparseSeriesIsOneLine(t *testing.T) {
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	// 24 часа по 12-минутным корзинам; данные — раз в час (каждая 5-я корзина).
	var points []metric.Point
	for i := 0; i < 120; i++ {
		p := metric.Point{T: base.Add(time.Duration(i) * 12 * time.Minute), V: math.NaN()}
		if i%5 == 0 {
			p.V = 100 + float64(i)
		}
		points = append(points, p)
	}
	out := metricSeriesMarkup(context.Background(), points, "ms", nil, nil, 720, 200)
	if got := strings.Count(out, "<polyline"); got != 1 {
		t.Errorf("разрежённый ряд должен давать одну линию, получено %d polyline\n%s", got, out)
	}
}

// пропуск заметно больше обычного интервала (простой приложения) должен
// остаться разрывом, мост через короткие пропуски его не маскирует.
func TestMetricSeriesRealGapBreaksLine(t *testing.T) {
	base := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	var points []metric.Point
	for i := 0; i < 120; i++ {
		p := metric.Point{T: base.Add(time.Duration(i) * 12 * time.Minute), V: math.NaN()}
		// Данные раз в час, но с 40-й по 80-ю корзину (8 часов) их нет вовсе.
		if i%5 == 0 && (i < 40 || i > 80) {
			p.V = 100 + float64(i)
		}
		points = append(points, p)
	}
	out := metricSeriesMarkup(context.Background(), points, "ms", nil, nil, 720, 200)
	if got := strings.Count(out, "<polyline"); got != 2 {
		t.Errorf("длинный пропуск должен рвать линию надвое, получено %d polyline\n%s", got, out)
	}
}

// плотный ряд (данные в каждой корзине) не переписывается — одиночная
// пустая корзина в нём остаётся разрывом.
func TestBridgeSparseGapsKeepsDenseSeries(t *testing.T) {
	pts := make([]seriesPoint, 10)
	for i := range pts {
		pts[i] = seriesPoint{x: float64(i), y: 1, has: i != 5}
	}
	got := bridgeSparseGaps(pts)
	if len(got) != len(pts) {
		t.Fatalf("плотный ряд не должен переписываться: было %d точек, стало %d", len(pts), len(got))
	}
	if got[5].has {
		t.Errorf("пустая корзина плотного ряда обязана остаться разрывом: %+v", got[5])
	}
}

// подписи дней должны звать тот же xLabelPlacement, что и writeXTicks —
// своя копия порога не учитывала сдвиг видимого центра при якоре start.
func TestChartBarsDayLabelsShareXLabelPlacement(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	points := []event.Point{
		{T: base, N: 3},
		{T: base.AddDate(0, 0, 1), N: 5},
		{T: base.AddDate(0, 0, 2), N: 4},
	}
	// narrowW в окне ~88-140: только на таком узком холсте вторая подпись
	// обязана переключиться на anchor start, не наехав на первую.
	const narrowW = 100
	out := chartBars(context.Background(), points, narrowW, chartHeight)

	// подписи дней на одной y=173.0 — этим отличаются от подписей оси Y и <title>.
	dayLabelRe := regexp.MustCompile(`<text x="([-\d.]+)" y="173\.0" text-anchor="(\w+)" fill="currentColor">`)
	matches := dayLabelRe.FindAllStringSubmatch(out, -1)
	if len(matches) < 2 {
		t.Fatalf("ожидались минимум 2 подписи дней, получено %d: %s", len(matches), out)
	}
	x0, err0 := strconv.ParseFloat(matches[0][1], 64)
	x1, err1 := strconv.ParseFloat(matches[1][1], 64)
	if err0 != nil || err1 != nil {
		t.Fatalf("не удалось распарсить x подписей: %v / %v", err0, err1)
	}
	anchor0, anchor1 := matches[0][2], matches[1][2]
	if anchor0 != "start" {
		t.Fatalf("якорь первой подписи дня = %q, ожидался start (стоит ровно на x0)", anchor0)
	}
	if anchor1 != "start" {
		t.Fatalf("якорь второй подписи дня = %q, ожидался start (сдвиг первой не учтён — наехала бы)", anchor1)
	}

	width0 := estimateTextWidth(narrowW, "01.07")
	rightFirst := x0 + width0 // якорь start растёт вправо от x
	leftSecond := x1          // тоже start — левый край подписи = сама x
	if gap := leftSecond - rightFirst; gap < 0 {
		t.Errorf("подписи дней перекрываются: правый край первой %.2f > левый край второй %.2f (%.2f/%s vs %.2f/%s)",
			rightFirst, leftSecond, x0, anchor0, x1, anchor1)
	}
}

// инвариант обязан держаться на КАЖДОЙ длине 1..90 (90 дней — retention),
// не в паре точек: у единого шага есть длины, куда не попасть без разрыва.
func TestDayLabelIndicesNeverExceedsTargetAcrossAllLengths(t *testing.T) {
	const target = 7
	for n := 1; n <= 90; n++ {
		got := dayLabelIndices(n, target)
		if len(got) > target {
			t.Errorf("dayLabelIndices(%d, %d) = %d меток, превышает цель %d", n, target, len(got), target)
		}
		seen := map[int]bool{}
		for _, idx := range got {
			if idx < 0 || idx >= n {
				t.Fatalf("dayLabelIndices(%d, %d) вернул индекс %d вне [0,%d)", n, target, idx, n)
			}
			if seen[idx] {
				t.Fatalf("dayLabelIndices(%d, %d) вернул повторяющийся индекс %d", n, target, idx)
			}
			seen[idx] = true
		}
	}
}

func TestDayLabelIndicesFixesOriginalUndercount(t *testing.T) {
	// восьмидневное окно исходно давало 4 подписи вместо заявленных 7 —
	// раскладка обязана вернуться близко к цели, не только не превысить её.
	got := dayLabelIndices(8, 7)
	if len(got) != 7 {
		t.Errorf("dayLabelIndices(8, 7) = %d меток, want 7", len(got))
	}
	if got[0] != 0 || got[len(got)-1] != 7 {
		t.Errorf("dayLabelIndices(8, 7) = %v, границы окна (0 и 7) обязаны быть в выборке", got)
	}
}
