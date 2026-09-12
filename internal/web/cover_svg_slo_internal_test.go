package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

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
	if strings.Contains(out, "chart-axis") {
		t.Errorf("оси нарисованы при отсутствии данных, ожидался ранний выход: %s", out)
	}
}

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
	if n := strings.Count(out, "chart-hover-band") + strings.Count(out, "<title>"); n == 0 {
		t.Errorf("нет полос наведения: %s", out)
	}
}

func TestSLOBudgetBurndownMarkupOverspend(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	points := []slo.Bucket{
		{T: base, Good: 500, Total: 1000},
		{T: base.Add(time.Hour), Good: 999, Total: 1000},
	}
	out := sloBudgetBurndownMarkup(context.Background(), points, 0.99, 1200, 260)
	if !strings.Contains(out, "slo-burndown-overspend") {
		t.Errorf("зона перерасхода не нарисована при отрицательном остатке: %s", out)
	}
}

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
