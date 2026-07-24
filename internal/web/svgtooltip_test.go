package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestChartsHaveTooltips — у каждого графика должна быть подсказка со
// значением: без неё пользователь видит форму линии, но не может прочитать
// величину ни в одной точке.
func TestChartsHaveTooltips(t *testing.T) {
	base := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)

	t.Run("график метрики", func(t *testing.T) {
		points := []metric.Point{{T: base, V: 738}, {T: base.Add(time.Hour), V: 1024}}
		out := metricSeriesMarkup(context.Background(), points, "ms", nil, 720, 200)
		if !strings.Contains(out, "hover-band") {
			t.Errorf("нет полос наведения: %s", out)
		}
		for _, want := range []string{"20.07 14:00", "738 ms"} {
			if !strings.Contains(out, want) {
				t.Errorf("подсказка без %q", want)
			}
		}
	})

	t.Run("стек задержек монитора", func(t *testing.T) {
		points := []uptime.LatencyPoint{{
			T: base, AvgTotalMs: 180, AvgDNSMs: 20, AvgConnectMs: 40, AvgTLSMs: 60, AvgTTFBMs: 60,
		}}
		out := latencyStackedMarkup(context.Background(), points, 480, 160)
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
		out := latencyStackedMarkup(context.Background(), points, 480, 160)
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
		out := sparklinePolyline([]uint64{3, 12, 7}, 96, 24, nil)
		for _, want := range []string{"min 3", "max 12", "· 7"} {
			if !strings.Contains(out, want) {
				t.Errorf("сводка без %q: %s", want, out)
			}
		}
	})

	t.Run("спарклайн латентности приводит микросекунды", func(t *testing.T) {
		out := sparklinePolyline([]uint64{50_000}, 96, 24, func(v uint64) string {
			return formatUSAxis(float64(v))
		})
		if !strings.Contains(out, "50ms") {
			t.Errorf("ожидалось приведение к ms: %s", out)
		}
	})
}
