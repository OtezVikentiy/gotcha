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

// TestMetricSeriesMarkupAxes — график метрики рисует оси (подписи значений +
// времени) и пороговую линию алерта в пределах области.
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
		`class="metric-chart chart-vb720"`, // + класс ширины viewBox: от неё зависит кегль подписей
		`class="chart-axis"`,
		`class="chart-threshold"`,
		`stroke-dasharray`, // пунктир пороговой линии
		`<polyline`,        // линия данных
		"10:00",            // подпись времени первой точки
		"ms",               // юнит в подписи оси Y
		"&gt; 30 ms",       // подпись порога с направлением сравнения (html-экранирован)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metric chart markup missing %q\n%s", want, out)
		}
	}
}

// TestMetricSeriesMarkupEmpty — пустой ряд рисует оси и заметку «нет данных»,
// а не падает и не оставляет голый холст.
func TestMetricSeriesMarkupEmpty(t *testing.T) {
	out := metricSeriesMarkup(context.Background(), nil, "", nil, nil, 720, 200)
	if !strings.Contains(out, "chart-axis") {
		t.Errorf("empty metric chart should still draw axes: %s", out)
	}
	if !strings.Contains(out, "нет данных") {
		t.Errorf("empty metric chart should note absence of data: %s", out)
	}
}

// TestMetricSeriesMarkupThresholdOutOfRange — порог далеко за пределами домена
// значений (после паддинга) не рисует линию, но и не ломает график.
func TestMetricSeriesMarkupThresholdInDomain(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	points := []metric.Point{{T: base, V: 10}, {T: base.Add(time.Hour), V: 20}}
	// Порог включён в домен, поэтому линия обязана присутствовать.
	out := metricSeriesMarkup(context.Background(), points, "", []metricThreshold{{Value: 15, Comparator: "lt"}}, nil, 720, 200)
	if !strings.Contains(out, "chart-threshold") {
		t.Errorf("threshold within data range must be drawn: %s", out)
	}
	if !strings.Contains(out, "&lt; 15") {
		t.Errorf("lt threshold label должен использовать знак <: %s", out)
	}
}

// TestMetricSeriesMarkupIgnoresInf — P2-14: домен Y фильтрует NaN, но должен
// так же фильтровать ±Inf. Одна Inf-точка иначе сделала бы domMax-domMin
// бесконечным и NaN-координаты получили бы ВСЕ точки, а не только сбойная —
// весь график сломался бы, а не одна точка на нём.
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
	// Домен должен остаться ограниченным конечными точками (10..25), не
	// растянутым в бесконечность.
	if !strings.Contains(out, "10 ms") || !strings.Contains(out, "25 ms") {
		t.Errorf("finite domain from non-Inf points expected: %s", out)
	}
}

// TestMetricSeriesMarkupFlatSeriesSingleYLabel — P2-15: плоский ряд
// (dataMin == dataMax) с alert-threshold, отличным от значения ряда, рисовал
// три Y-подписи (max/середина/min), совпадающие по значению и координате —
// три наложенных друг на друга подписи и линии. Для плоского ряда должна
// остаться одна подпись.
func TestMetricSeriesMarkupFlatSeriesSingleYLabel(t *testing.T) {
	base := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	points := []metric.Point{{T: base, V: 5}, {T: base.Add(time.Hour), V: 5}}
	thresholds := []metricThreshold{{Value: 20, Comparator: "gt"}}
	out := metricSeriesMarkup(context.Background(), points, "ms", thresholds, nil, 720, 200)

	// Только подписи оси Y используют этот атрибутный набор — в отличие от
	// hover-band тултипов (<title>…) и порога, которые тоже могут содержать
	// "5 ms"/"20 ms" в тексте.
	if got := strings.Count(out, `dominant-baseline="middle" fill="currentColor">5 ms</text>`); got != 1 {
		t.Errorf("flat series must draw a single Y-axis label, got %d: %s", got, out)
	}
}

// TestChartBarsAxes — график частоты событий рисует оси и подписи максимума и
// времени, столбики попадают в область графика.
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
		`<rect`,   // столбики
		">0<",     // нижняя линия сетки
		">10<",    // верх шкалы: на шаг выше максимума (max=7, шаг 5)
		">5<",     // промежуточная линия сетки
		"18.07",   // подпись дня
		"21.07",   // подпись следующего дня — метки ставятся на каждой границе суток
		"<title>", // подсказка при наведении
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frequency chart markup missing %q\n%s", want, out)
		}
	}
}

// TestChartBarsTooltip — у каждого столбика своя подсказка со временем корзины
// и количеством: без неё значение столбика ниоткуда не прочитать, а
// оформленной подсказки в проекте нет (она требовала бы JS).
func TestChartBarsTooltip(t *testing.T) {
	base := time.Date(2026, 7, 18, 15, 0, 0, 0, time.UTC)
	out := chartBars(context.Background(), []event.Point{{T: base, N: 5}}, chartWidth, chartHeight)

	if strings.Count(out, "<title>") != 1 {
		t.Errorf("ожидалась одна подсказка на один столбик: %s", out)
	}
	// K9-15: время подсказки — через humanize.Time (дата, время, пояс).
	for _, want := range []string{"2026-07-18 15:00 UTC", "5 событий"} {
		if !strings.Contains(out, want) {
			t.Errorf("подсказка без %q: %s", want, out)
		}
	}
}

// TestNiceStep — шаг сетки берётся из ряда 1/2/5×10ⁿ, иначе подписи оси
// получаются вида 37/74/111 и прикинуть по ним значение нельзя.
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

// TestChartBarsHeadroom — верх шкалы всегда строго выше максимума: когда они
// совпадают, самый высокий столбик упирается в рамку и график выглядит
// сплошным забором.
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

// TestMetricSeriesSparseSeriesIsOneLine — ряд, который приходит реже сетки
// корзин (метрика раз в час при 12-минутном шаге), обязан рисоваться ОДНОЙ
// линией, а не рассыпаться на изолированные отметки: до правки каждая непустая
// корзина оказывалась одиночным сегментом и график превращался в «лес спичек».
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

// TestMetricSeriesRealGapBreaksLine — пропуск, заметно больший обычного
// интервала ряда (простой приложения), обязан остаться РАЗРЫВОМ: мост через
// короткие пропуски не должен маскировать отсутствие данных.
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

// TestBridgeSparseGapsKeepsDenseSeries — плотный ряд (данные в каждой корзине)
// правка не трогает: одиночная пустая корзина в нём — настоящий провал и
// обязана остаться разрывом.
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

// TestChartBarsDayLabelsShareXLabelPlacement — подписи дней chartBars должны
// звать ту же xLabelPlacement, что и writeXTicks (svgaxis.go), а не
// собственную копию порога у края холста (P1-7): на узком холсте первая
// подпись дня стоит ровно на x0 и переключается на якорь "start", что
// сдвигает её видимый центр на полширины вправо; вторая подпись (день
// спустя, рядом с первой) должна это учесть, иначе наезжает на первую — как
// у TestWriteXTicksAccountsForAnchorShift для writeXTicks. Собственная копия
// порога (было: фиксированные 24 у обоих генераторов) этот сдвиг не
// учитывала.
func TestChartBarsDayLabelsShareXLabelPlacement(t *testing.T) {
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	points := []event.Point{
		{T: base, N: 3},
		{T: base.AddDate(0, 0, 1), N: 5},
		{T: base.AddDate(0, 0, 2), N: 4},
	}
	// x0=chartPadL=40, x1=narrowW-chartPadR=56 — дни ложатся вплотную. Ширина
	// руны пропорциональна холсту (svgCharWidthPerVB), поэтому «близко»
	// достигается только очень узким холстом: подпись дня (5 рун) с якорем
	// middle наезжает на первую при w < ~70, а починить это сменой якоря на
	// start ещё можно при w ≥ ~62 (x второй ≥ правого края первой) — окно
	// 62..69, иначе вторая подпись подавляется вовсе.
	const narrowW = 66
	out := chartBars(context.Background(), points, narrowW, chartHeight)

	// Подписи дней рисуются на одной и той же y = chartHeight-7 = 173.0 —
	// этим отличаются от подписей оси Y (другой y) и подсказок <title>.
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
