package web

import (
	"context"
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
	out := metricSeriesMarkup(context.Background(), points, "ms", thresholds, 720, 200)

	for _, want := range []string{
		`class="metric-chart"`,
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
	out := metricSeriesMarkup(context.Background(), nil, "", nil, 720, 200)
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
	out := metricSeriesMarkup(context.Background(), points, "", []metricThreshold{{Value: 15, Comparator: "lt"}}, 720, 200)
	if !strings.Contains(out, "chart-threshold") {
		t.Errorf("threshold within data range must be drawn: %s", out)
	}
	if !strings.Contains(out, "&lt; 15") {
		t.Errorf("lt threshold label должен использовать знак <: %s", out)
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
		`class="chart-freq"`,
		`class="chart-axis"`,
		`<rect`, // столбики
		">0<",   // нижняя линия сетки
		">10<",  // верх шкалы: на шаг выше максимума (max=7, шаг 5)
		">5<",   // промежуточная линия сетки
		"18.07", // подпись дня
		"21.07", // подпись следующего дня — метки ставятся на каждой границе суток
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
	for _, want := range []string{"18.07 15:00", "5 событий"} {
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
