package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestChartsHaveTooltips — у каждого графика должна быть подсказка со
// значением: без неё пользователь видит форму линии, но не может прочитать
// величину ни в одной точке.
func TestChartsHaveTooltips(t *testing.T) {
	base := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)

	t.Run("график метрики", func(t *testing.T) {
		points := []metric.Point{{T: base, V: 738}, {T: base.Add(time.Hour), V: 1024}}
		out := metricSeriesMarkup(context.Background(), points, "ms", nil, nil, 720, 200)
		if !strings.Contains(out, "hover-band") {
			t.Errorf("нет полос наведения: %s", out)
		}
		// K9-15: время в подсказке — через humanize.Time, с подписью пояса,
		// а не «20.07 14:00» без года и зоны.
		for _, want := range []string{"2026-07-20 14:00 UTC", "738 ms"} {
			if !strings.Contains(out, want) {
				t.Errorf("подсказка без %q: %s", want, out)
			}
		}
	})

	t.Run("стек задержек монитора", func(t *testing.T) {
		points := []uptime.LatencyPoint{{
			T: base, AvgTotalMs: 180, AvgDNSMs: 20, AvgConnectMs: 40, AvgTLSMs: 60, AvgTTFBMs: 60,
		}}
		out := latencyStackedMarkup(context.Background(), points, nil, 480, 160)
		for _, want := range []string{"<title>", "DNS 20ms", "180ms", "chart-axis", "hover-band"} {
			if !strings.Contains(out, want) {
				t.Errorf("подсказка без %q: %s", want, out)
			}
		}
	})

	t.Run("час с таймаутом не ломает шкалу, помечен меткой", func(t *testing.T) {
		// Здоровые часы ~90мс задают шкалу; час с таймаутом (фазы 0, total
		// 30000мс) не должен её вытягивать, но обязан быть виден как выброс.
		points := []uptime.LatencyPoint{
			{T: base, AvgTotalMs: 90, AvgDNSMs: 10, AvgConnectMs: 20, AvgTLSMs: 25, AvgTTFBMs: 30},
			{T: base.Add(time.Hour), AvgTotalMs: 30000},
			{T: base.Add(2 * time.Hour), AvgTotalMs: 95, AvgDNSMs: 12, AvgConnectMs: 21, AvgTLSMs: 26, AvgTTFBMs: 31},
		}
		out := latencyStackedMarkup(context.Background(), points, nil, 480, 160)
		if strings.Contains(out, "30000ms") == false {
			t.Errorf("нет полного total выброса в подсказке: %s", out)
		}
		if !strings.Contains(out, "seg-cap") {
			t.Errorf("нет метки выброса seg-cap: %s", out)
		}
		if !strings.Contains(out, "выше шкалы") {
			t.Errorf("нет пометки «выше шкалы»: %s", out)
		}
		// Шкала не должна доходить до секунд: верх ~100мс, значит на оси есть
		// подпись в мс и нет «30s».
		if strings.Contains(out, "30s") || strings.Contains(out, "30.0s") {
			t.Errorf("выброс вытянул шкалу до секунд: %s", out)
		}
	})

	t.Run("спарклайн", func(t *testing.T) {
		out := sparklinePolyline(context.Background(), []uint64{3, 12, 7}, 96, 24, nil)
		for _, want := range []string{"min 3", "max 12", "· 7"} {
			if !strings.Contains(out, want) {
				t.Errorf("сводка без %q: %s", want, out)
			}
		}
	})

	t.Run("спарклайн латентности приводит микросекунды", func(t *testing.T) {
		out := sparklinePolyline(context.Background(), []uint64{50_000}, 96, 24, func(v uint64) string {
			return formatUSAxis(float64(v))
		})
		if !strings.Contains(out, "50ms") {
			t.Errorf("ожидалось приведение к ms: %s", out)
		}
	})
}

// TestChartTooltipsUseHumanizeTime — K9-15: все подсказки графиков печатают
// время одним видом с остальным интерфейсом (humanize.Time: дата, время,
// подпись пояса), а не «02.01 15:04» мимо humanize. Подписи оси X (короткие
// «02.01»/«15:04») сюда не входят — это не подсказка.
func TestChartTooltipsUseHumanizeTime(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	const stamp = "2026-07-20 14:00 UTC"
	lp := []trace.LatencyPoint{{T: base, P50: 100, P95: 200, Count: 5}, {T: base.Add(time.Hour), P50: 110, P95: 210, Count: 6}}
	outs := map[string]string{
		"latencyLinesMarkup":   latencyLinesMarkup(ctx, lp, nil, 720, 200),
		"throughputBarsMarkup": throughputBarsMarkup(ctx, lp, nil, 720, 200),
		"chartBars":            chartBars(ctx, []event.Point{{T: base, N: 3}, {T: base.Add(24 * time.Hour), N: 5}}, 720, 200),
		"latencyStackedMarkup": latencyStackedMarkup(ctx, []uptime.LatencyPoint{{T: base, AvgTotalMs: 180, AvgDNSMs: 20, AvgConnectMs: 40, AvgTLSMs: 60, AvgTTFBMs: 60}}, nil, 480, 160),
		"logHistogramMarkup":   logHistogramMarkup(ctx, []time.Time{base, base.Add(time.Hour)}, logSeriesAllSeverities(2, []int64{2, 3}), 720, 200),
	}
	for name, out := range outs {
		if !strings.Contains(out, stamp) {
			t.Errorf("%s: подсказка без %q: %s", name, stamp, out)
		}
		if strings.Contains(out, "20.07 14:00") {
			t.Errorf("%s: подсказка по-прежнему в формате «02.01 15:04»: %s", name, out)
		}
	}

	// vitalSeriesMarkup: в <title> — границы диапазона, обе через humanize.
	vital := vitalSeriesMarkup(ctx, []trace.VitalPoint{{T: base, P75: 1.5}, {T: base.Add(48 * time.Hour), P75: 2.5}}, 720, 200, func(v float64) string { return "x" })
	if !strings.Contains(vital, stamp+" – 2026-07-22 14:00 UTC") {
		t.Errorf("vitalSeriesMarkup: границы диапазона не через humanize.Time: %s", vital)
	}
}

// logSeriesAllSeverities — серии гистограммы логов: logHistogramMarkup
// индексирует series[sev][i] по всему log.Severities, поэтому у каждой
// серьёзности обязан быть срез длины n; ненулевые значения — только у error.
func logSeriesAllSeverities(n int, errorCounts []int64) map[string][]int64 {
	series := map[string][]int64{}
	for _, sev := range log.Severities {
		series[sev] = make([]int64, n)
	}
	copy(series[log.SevError], errorCounts)
	return series
}
