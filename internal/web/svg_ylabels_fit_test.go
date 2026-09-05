package web

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// yAxisLabelRe — подписи оси Y: якорь end + dominant-baseline=middle, этим
// они отличаются от подписей оси X и версий деплоя.
var yAxisLabelRe = regexp.MustCompile(`<text x="([-\d.]+)" y="[-\d.]+" text-anchor="end" dominant-baseline="middle" fill="currentColor">([^<]*)</text>`)

// logSeriesFixture — ряды объёма логов по всем severity (как отдаёт
// log.Query.Histogram: добиты нулями), info с пиком peak в первой корзине.
func logSeriesFixture(n int, peak int64) map[string][]int64 {
	series := map[string][]int64{}
	for _, sev := range log.Severities {
		series[sev] = make([]int64, n)
	}
	if n > 0 {
		series[log.SevInfo][0] = peak
	}
	if n > 1 {
		series[log.SevInfo][1] = peak / 2
	}
	return series
}

// yAxisLabelSites — сколько мест в svg*.go рисуют подписи оси Y (якорь end +
// dominant-baseline=middle): writeYGrid (svgaxis.go), chartBars ×2 (пустой и
// обычный), metricSeriesMarkup (svg.go), sloBudgetBurndownMarkup (svg_slo.go).
// Новое место обязано попасть и сюда, и в таблицу теста ниже — сторож
// TestYAxisLabelSitesCovered считает их по исходникам.
const yAxisLabelSites = 5

// TestChartsYLabelsFitAtTierWidth — каждый генератор графика с осью Y
// раздвигает левое поле под свои подписи (fitYLabels/yAxisPadL) ДО
// рисования: ни одна подпись не выходит за левый край вьюбокса на
// калиброванном тире. Фикстуры подобраны так, чтобы подписи были шире поля
// вызывающего (48-64): «200ms»/«400ms» на chart-vb1200 ≈75 единиц, «500ms»
// на chart-vb720 ≈45+6 > 48, «10000» на логах, «1000»+ на счётчиках. Тест
// на сгенерированный SVG, а не на fitYLabels напрямую: именно вызов в
// каждом генераторе — то, что легко потерять.
func TestChartsYLabelsFitAtTierWidth(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Hour), base.Add(2 * time.Hour)}
	latency := []trace.LatencyPoint{
		{T: times[0], P50: 100_000, P95: 400_000, Count: 1500},
		{T: times[1], P50: 120_000, P95: 380_000, Count: 900},
		{T: times[2], P50: 90_000, P95: 410_000, Count: 1200},
	}
	cases := []struct {
		name string
		w    int
		padL float64 // поле вызывающего — фикстура обязана его превышать
		svg  string
	}{
		{"перцентили эндпойнта", perfLatencyChartWidth, 64,
			latencyLinesMarkup(ctx, latency, nil, perfLatencyChartWidth, perfLatencyChartHeight)},
		{"throughput эндпойнта", perfLatencyChartWidth, 48,
			throughputBarsMarkup(ctx, latency, nil, perfLatencyChartWidth, perfLatencyChartHeight)},
		{"гистограмма длительностей", perfLatencyChartWidth, 48,
			durationHistogramMarkup(ctx, []trace.DurationBucket{{UpperUS: 10_000, Count: 1500}, {UpperUS: 20_000, Count: 300}}, perfLatencyChartWidth, 200)},
		{"задержки монитора", latencyChartWidth, 48,
			latencyStackedMarkup(ctx, []uptime.LatencyPoint{
				{T: times[0], AvgTotalMs: 1500, AvgDNSMs: 100, AvgConnectMs: 200, AvgTLSMs: 300, AvgTTFBMs: 900},
				{T: times[1], AvgTotalMs: 1200, AvgDNSMs: 100, AvgConnectMs: 200, AvgTLSMs: 300, AvgTTFBMs: 600},
			}, nil, latencyChartWidth, latencyChartHeight)},
		{"объём логов", latencyChartWidth, 48,
			logHistogramMarkup(ctx, times, logSeriesFixture(len(times), 15000), latencyChartWidth, latencyChartHeight)},
		{"графики карточки хоста", hostChartWidth, 58,
			multiSeriesMarkup(ctx, []NamedSeries{{Label: "cpu", Points: []metric.Point{{T: times[0], V: 200}, {T: times[1], V: 400}, {T: times[2], V: 350}}}},
				"ms", nil, nil, hostChartWidth, 200)},
		{"ряд на странице метрики", metricChartWidth, 58,
			metricSeriesMarkup(ctx, []metric.Point{{T: times[0], V: 200}, {T: times[1], V: 400}, {T: times[2], V: 350}},
				"ms", nil, nil, metricChartWidth, 200)},
		{"burn-down бюджета SLO", sloBurndownWidth, 58,
			sloBudgetBurndownMarkup(ctx, []slo.Bucket{{T: times[0], Good: 99, Total: 100}, {T: times[1], Good: 98, Total: 100}, {T: times[2], Good: 100, Total: 100}},
				0.99, sloBurndownWidth, 260)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			labels := yAxisLabelRe.FindAllStringSubmatch(c.svg, -1)
			if len(labels) < 2 {
				t.Fatalf("подписей оси Y %d, ожидалось ≥2: %s", len(labels), c.svg)
			}
			wide := false
			for _, m := range labels {
				x, err := strconv.ParseFloat(m[1], 64)
				if err != nil {
					t.Fatalf("x подписи %q: %v", m[1], err)
				}
				width := estimateTextWidth(c.w, m[2])
				if left := x - width; left < -0.05 {
					t.Errorf("подпись %q на x=%.2f обрезана слева на %.2f единиц — поле под ось Y не раздвинуто", m[2], x, -left)
				}
				if width+yLabelGap > c.padL {
					wide = true
				}
			}
			if !wide {
				t.Fatalf("фикстура сломана: ни одна подпись не шире поля вызывающего (%.0f) — тест ничего не проверяет: %v", c.padL, labels)
			}
		})
	}
}

// TestYAxisLabelSitesCovered — сторож исчерпывающести таблицы выше: число
// мест в svg*.go, печатающих подпись оси Y (якорь end + dominant-baseline=
// middle), обязано совпадать с yAxisLabelSites. Новый генератор с осью Y
// краснит тест, пока не будет добавлен в TestChartsYLabelsFitAtTierWidth
// (и не получит yAxisPadL/fitYLabels), — ревью задачи 9 нашло седьмой
// генератор (SLO) именно потому, что список велся по памяти.
func TestYAxisLabelSitesCovered(t *testing.T) {
	files, err := filepath.Glob("svg*.go")
	if err != nil {
		t.Fatal(err)
	}
	const marker = `text-anchor="end" dominant-baseline="middle"`
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), marker); n > 0 {
			t.Logf("%s: %d", f, n)
			total += n
		}
	}
	if total != yAxisLabelSites {
		t.Fatalf("мест с подписью оси Y в svg*.go = %d, в yAxisLabelSites записано %d — "+
			"новое место добавить в TestChartsYLabelsFitAtTierWidth и подключить yAxisPadL", total, yAxisLabelSites)
	}
}
