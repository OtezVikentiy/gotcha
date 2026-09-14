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

// якорь end + dominant-baseline=middle отличает подписи оси Y от оси X и версий деплоя.
var yAxisLabelRe = regexp.MustCompile(`<text x="([-\d.]+)" y="[-\d.]+" text-anchor="end" dominant-baseline="middle" fill="currentColor">([^<]*)</text>`)

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

// новое место с подписью оси Y — сюда и в таблицу теста ниже, иначе
// просядет TestYAxisLabelSitesCovered.
const yAxisLabelSites = 5

// проверяем сгенерированный SVG, не fitYLabels напрямую — забыть вызвать
// его в конкретном генераторе легко.
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

var chartVBMinWidthRe = regexp.MustCompile(`^@media \(min-width: (\d+)px\)$`)

// @media (max-width: Nxpx), под которым живёт clamp min-width:480px — верхняя граница
// мобильной компенсации кегля (issue-chart/endpoint-chart/metric-chart-wrap/slo-burndown/
// waterfall). Именно она и обязана стыковаться с первым widescreen-тиром без разрыва.
var mobileClampMediaRe = regexp.MustCompile(`(?s)@media \(max-width: (\d+)px\)\s*\{(.*?)\n\}`)

// находка N12: между верхней границей мобильной компенсации (≤560) и первым
// widescreen-тиром .chart-vbN (было ≥700) оставался диапазон на безмедийной ступени —
// её кегль вдвое крупнее любого соседа. Сторож ловит класс проблемы (разрыв между
// границами), а не конкретные числа, поэтому переживёт сдвиг любого из брейкпоинтов.
func TestChartVBTierStartsRightAfterMobileClamp(t *testing.T) {
	css, err := readAppCSS()
	if err != nil {
		t.Fatalf("читаю app.css: %v", err)
	}
	css = cssCommentRe.ReplaceAllString(css, " ")

	mobileMax := -1
	for _, m := range mobileClampMediaRe.FindAllStringSubmatch(css, -1) {
		if !strings.Contains(m[2], "min-width: 480px") {
			continue
		}
		if w, err := strconv.Atoi(m[1]); err == nil && w > mobileMax {
			mobileMax = w
		}
	}
	if mobileMax < 0 {
		t.Fatal("в app.css нет @media (max-width: NNNpx) с min-width:480px — clamp мобильной компенсации потерян")
	}

	tierMin := -1
	for _, ctxs := range chartVBTextContexts(css) {
		for c := range ctxs {
			m := chartVBMinWidthRe.FindStringSubmatch(c)
			if m == nil {
				continue
			}
			if w, err := strconv.Atoi(m[1]); err == nil && (tierMin == -1 || w < tierMin) {
				tierMin = w
			}
		}
	}
	if tierMin == -1 {
		t.Fatal("нет ни одного min-width-тира .chart-vbN text")
	}

	if tierMin != mobileMax+1 {
		t.Errorf("первый widescreen-тир .chart-vbN начинается с %dpx, мобильная компенсация действует до %dpx — "+
			"диапазон %d-%dpx остаётся на безмедийной (базовой) ступени без компенсации",
			tierMin, mobileMax, mobileMax+1, tierMin-1)
	}
}
