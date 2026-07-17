package web

import (
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
	out := metricSeriesMarkup(points, "ms", thresholds, 720, 200)

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
	out := metricSeriesMarkup(nil, "", nil, 720, 200)
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
	out := metricSeriesMarkup(points, "", []metricThreshold{{Value: 15, Comparator: "lt"}}, 720, 200)
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
	out := chartBars(points, chartWidth, chartHeight)
	for _, want := range []string{
		`class="chart-freq"`,
		`class="chart-axis"`,
		`<rect`, // столбики
		">7<",   // подпись максимума на оси Y
		"18.07", // подпись даты (окно > 48ч → день.месяц)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("frequency chart markup missing %q\n%s", want, out)
		}
	}
}

// TestChartBarsEmpty — пустой ряд рисует оси с нулём, без паники.
func TestChartBarsEmpty(t *testing.T) {
	out := chartBars(nil, chartWidth, chartHeight)
	if !strings.Contains(out, "chart-axis") {
		t.Errorf("empty frequency chart should draw axes: %s", out)
	}
}
