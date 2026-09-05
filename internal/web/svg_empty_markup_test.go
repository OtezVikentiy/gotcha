package web

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

// TestChartsEmptyMarkupIsBalanced — все восемь генераторов с осью Y на ПУСТЫХ
// данных отдают валидную разметку: один <svg>…</svg>, открывающих <g> ровно
// столько же, сколько закрывающих, никаких тегов после </svg>. Ревью задачи 9
// поймало у metricSeriesMarkup осиротевший </g>: группа осей стала
// открываться после расчёта поля под подписи, а ранний выход по пустым
// данным остался выше — такой тест был только у метрики, и дефект уехал.
// Где генератор в пустой ветке рисует рамку осей (flag axes: metricSeries и
// chartBars), проверяется и она: две линии внутри <g class="chart-axis">.
// Остальные на пустых данных рамку не рисуют (плейсхолдер у multiSeries/SLO,
// флэтлайн у latencyLines, одна базовая линия chartEmptyAxis у столбчатых —
// поведение до задачи 9), у них проверяется только баланс.
func TestChartsEmptyMarkupIsBalanced(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		axes bool
		svg  string
	}{
		{"ряд на странице метрики", true, metricSeriesMarkup(ctx, nil, "", nil, nil, metricChartWidth, 200)},
		{"ряд на странице метрики (только NaN)", true,
			metricSeriesMarkup(ctx, []metric.Point{{T: time.Now(), V: math.NaN()}}, "", nil, nil, metricChartWidth, 200)},
		{"графики карточки хоста", false, multiSeriesMarkup(ctx, nil, "", nil, nil, hostChartWidth, 200)},
		{"частота событий issue", true, chartBars(ctx, nil, chartWidth, chartHeight)},
		{"частота событий issue (нули)", true, chartBars(ctx, []event.Point{{T: time.Now(), N: 0}}, chartWidth, chartHeight)},
		{"перцентили эндпойнта", false, latencyLinesMarkup(ctx, nil, nil, perfLatencyChartWidth, perfLatencyChartHeight)},
		{"throughput эндпойнта", false, throughputBarsMarkup(ctx, nil, nil, perfLatencyChartWidth, perfLatencyChartHeight)},
		{"гистограмма длительностей", false, durationHistogramMarkup(ctx, nil, perfLatencyChartWidth, 200)},
		{"задержки монитора", false, latencyStackedMarkup(ctx, nil, nil, latencyChartWidth, latencyChartHeight)},
		{"объём логов", false, logHistogramMarkup(ctx, nil, logSeriesFixture(0, 0), latencyChartWidth, latencyChartHeight)},
		{"burn-down бюджета SLO", false, sloBudgetBurndownMarkup(ctx, []slo.Bucket{{T: time.Now()}}, 0.99, sloBurndownWidth, 260)},
		{"burn-down бюджета SLO (без корзин)", false, sloBudgetBurndownMarkup(ctx, nil, 0.99, sloBurndownWidth, 260)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := c.svg
			if strings.Count(out, "<svg") != 1 || strings.Count(out, "</svg>") != 1 || !strings.HasSuffix(out, "</svg>") {
				t.Fatalf("ожидался ровно один <svg>…</svg> и ничего после: %s", out)
			}
			open, closed := strings.Count(out, "<g "), strings.Count(out, "</g>")
			open += strings.Count(out, "<g>")
			if open != closed {
				t.Fatalf("дисбаланс групп: <g> ×%d, </g> ×%d — невалидный SVG: %s", open, closed, out)
			}
			if !c.axes {
				return
			}
			i := strings.Index(out, `<g class="chart-axis">`)
			if i < 0 {
				t.Fatalf("нет группы осей <g class=\"chart-axis\">: %s", out)
			}
			group := out[i:]
			if j := strings.Index(group, "</g>"); j >= 0 {
				group = group[:j]
			}
			if n := strings.Count(group, "<line "); n < 2 {
				t.Fatalf("рамка осей у пустого графика: ожидались ≥2 линии в группе осей, найдено %d: %s", n, out)
			}
		})
	}
}
