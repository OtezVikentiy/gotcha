package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

func TestSLOBurndownSVG(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	pts := []slo.Bucket{
		{T: base, Good: 100, Total: 100},
		{T: base.Add(time.Hour), Good: 100, Total: 100},
		{T: base.Add(2 * time.Hour), Good: 50, Total: 100},
	}
	out := sloBudgetBurndownMarkup(context.Background(), pts, 0.99, 1200, 260)
	if !strings.Contains(out, "<svg") {
		t.Fatalf("нет svg: %s", out)
	}
	if !strings.Contains(out, "slo-burndown-line") {
		t.Fatalf("нет линии остатка бюджета: %s", out)
	}
	if !strings.Contains(out, "slo-burndown-overspend") {
		t.Fatalf("остаток ушёл в минус, но зоны перерасхода нет: %s", out)
	}

	healthy := []slo.Bucket{
		{T: base, Good: 100, Total: 100},
		{T: base.Add(time.Hour), Good: 100, Total: 100},
	}
	hout := sloBudgetBurndownMarkup(context.Background(), healthy, 0.99, 1200, 260)
	if !strings.Contains(hout, "slo-burndown-line") {
		t.Fatalf("здоровый ряд без линии: %s", hout)
	}
	if strings.Contains(hout, "slo-burndown-overspend") {
		t.Fatalf("без перерасхода зона перерасхода не должна рисоваться: %s", hout)
	}

	empty := sloBudgetBurndownMarkup(context.Background(), nil, 0.99, 1200, 260)
	if !strings.Contains(empty, "<svg") {
		t.Fatalf("пустой ряд без svg: %s", empty)
	}
	if strings.Contains(empty, "slo-burndown-line") {
		t.Fatalf("пустой ряд не должен рисовать линию: %s", empty)
	}
}
