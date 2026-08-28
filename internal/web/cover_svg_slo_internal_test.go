package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

// TestSLOBudgetPct покрывает форматирование доли бюджета в проценты без
// дробной части: полный бюджет, исчерпанный и перерасход (отрицательный).
func TestSLOBudgetPct(t *testing.T) {
	for _, tc := range []struct {
		frac float64
		want string
	}{
		{1.0, "100%"},
		{0, "0%"},
		{-4.0, "-400%"},
		{0.5, "50%"},
	} {
		if got := sloBudgetPct(tc.frac); got != tc.want {
			t.Errorf("sloBudgetPct(%v) = %q, want %q", tc.frac, got, tc.want)
		}
	}
}

// TestSLOBudgetBurndownMarkupNoData — окно без единого события (Total==0 в
// каждой корзине): линия остатка не может быть посчитана НИ В ОДНОЙ точке
// префикса, график обязан показать заглушку «нет данных», а не пустой SVG или
// панику на делении на ноль total.
func TestSLOBudgetBurndownMarkupNoData(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	points := []slo.Bucket{
		{T: base, Good: 0, Total: 0},
		{T: base.Add(time.Hour), Good: 0, Total: 0},
	}
	out := sloBudgetBurndownMarkup(context.Background(), points, 0.99, 1200, 260)
	if !strings.HasSuffix(strings.TrimSpace(out), "</svg>") {
		t.Fatalf("markup не завершается </svg>: %s", out)
	}
	if strings.Contains(out, "<polyline") {
		t.Errorf("нет данных, но нарисована линия: %s", out)
	}
	if !strings.Contains(out, "<text") {
		t.Errorf("нет текстовой заглушки «нет данных»: %s", out)
	}
	// Заглушка — это ранний выход: осей/сетки/hover-полос без данных быть не
	// должно (иначе это не «нет данных», а обычный график с пустой линией).
	if strings.Contains(out, "chart-axis") {
		t.Errorf("оси нарисованы при отсутствии данных, ожидался ранний выход: %s", out)
	}
}

// TestSLOBudgetBurndownMarkupNoOverspend — бюджет ни разу не уходит в минус:
// красная зона перерасхода не должна рисоваться (иначе узкая полоска запаса
// пугала бы зря).
func TestSLOBudgetBurndownMarkupNoOverspend(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	points := []slo.Bucket{
		{T: base, Good: 999, Total: 1000},
		{T: base.Add(time.Hour), Good: 999, Total: 1000},
		{T: base.Add(2 * time.Hour), Good: 999, Total: 1000},
	}
	out := sloBudgetBurndownMarkup(context.Background(), points, 0.99, 1200, 260)
	if !strings.Contains(out, "<polyline") {
		t.Fatalf("линия остатка бюджета не нарисована: %s", out)
	}
	if strings.Contains(out, "slo-burndown-overspend") {
		t.Errorf("зона перерасхода нарисована без перерасхода: %s", out)
	}
	// Полосы наведения — по одной на корзину (все три с данными).
	if n := strings.Count(out, "chart-hover-band") + strings.Count(out, "<title>"); n == 0 {
		t.Errorf("нет полос наведения: %s", out)
	}
}

// TestSLOBudgetBurndownMarkupOverspend — накопленный остаток уходит в минус
// (перерасход бюджета): должна нарисоваться красная зона под нулевой линией.
func TestSLOBudgetBurndownMarkupOverspend(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	points := []slo.Bucket{
		// attainment всей корзины 500/1000=0.5, target 0.99 → consumed
		// огромный, remaining сильно отрицателен.
		{T: base, Good: 500, Total: 1000},
		{T: base.Add(time.Hour), Good: 999, Total: 1000},
	}
	out := sloBudgetBurndownMarkup(context.Background(), points, 0.99, 1200, 260)
	if !strings.Contains(out, "slo-burndown-overspend") {
		t.Errorf("зона перерасхода не нарисована при отрицательном остатке: %s", out)
	}
}

// TestSLOBudgetBurndownMarkupLeadingGap — до первого события накопленный
// total==0 (пустой префикс): эта корзина обязана стать разрывом линии
// (has=false), а не мнимым нулевым остатком, как только события начинаются —
// линия рисуется по остальным точкам.
func TestSLOBudgetBurndownMarkupLeadingGap(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	points := []slo.Bucket{
		{T: base, Good: 0, Total: 0},
		{T: base.Add(time.Hour), Good: 99, Total: 100},
		{T: base.Add(2 * time.Hour), Good: 99, Total: 100},
	}
	out := sloBudgetBurndownMarkup(context.Background(), points, 0.5, 1200, 260)
	if !strings.Contains(out, "<polyline") {
		t.Fatalf("данные есть, но линия не нарисована: %s", out)
	}
}
