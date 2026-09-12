package web

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestCoverSVGHelpers(t *testing.T) {
	if truncateRunes("hello", 0) != "" {
		t.Error("truncateRunes n<=0")
	}
	if truncateRunes("hi", 5) != "hi" {
		t.Error("truncateRunes short")
	}
	if truncateRunes("hello", 2) != "he" {
		t.Error("truncateRunes cut")
	}

	for _, tc := range []struct {
		v    float64
		unit string
	}{{2e6, ""}, {5000, "ms"}, {42, "1"}, {-3e6, ""}} {
		if formatAxisValue(tc.v, tc.unit) == "" {
			t.Errorf("formatAxisValue(%v,%q) empty", tc.v, tc.unit)
		}
	}

	if comparatorSymbol("lt") != "<" || comparatorSymbol("gt") != ">" {
		t.Error("comparatorSymbol branches")
	}

	if formatCoord(math.NaN()) != "0.0" || formatCoord(math.Inf(1)) != "0.0" {
		t.Error("formatCoord non-finite")
	}
	if formatCoord(1.5) != "1.5" {
		t.Error("formatCoord finite")
	}

	if niceStep(0, 4) != 1 || niceStep(3, 10) != 1 {
		t.Error("niceStep degenerate")
	}
	if niceStep(1000, 4) == 0 {
		t.Error("niceStep normal zero")
	}
	if niceStepFloat(0, 4) != 1 || niceStepFloat(-5, 4) != 1 {
		t.Error("niceStepFloat degenerate")
	}
	if niceStepFloat(2500, 4) <= 0 {
		t.Error("niceStepFloat normal")
	}

	fmtU := func(v uint64) string { return strconv.FormatUint(v, 10) }
	if sparklinePolyline(context.Background(), []uint64{0, 0, 0}, 40, 12, fmtU) == "" {
		t.Error("sparklinePolyline all-zero empty")
	}
	if sparklinePolyline(context.Background(), []uint64{1, 5, 3}, 40, 12, fmtU) == "" {
		t.Error("sparklinePolyline normal empty")
	}
}

func TestWriteLineWithAreaDrawsIsolatedPoint(t *testing.T) {
	pts := []seriesPoint{
		{x: 0, y: 50, has: false},
		{x: 10, y: 20, has: true},
		{x: 20, y: 50, has: false},
	}

	var sb strings.Builder
	writeLineWithArea(&sb, pts, 100, "#3d7bff", "gradTest", `stroke="#3d7bff"`)
	out := sb.String()

	if !strings.Contains(out, "<polyline") {
		t.Fatalf("одиночная корзина с данными не нарисована:\n%s", out)
	}
	if !strings.Contains(out, ",20.0") {
		t.Errorf("отметка не на значении точки (y=20):\n%s", out)
	}
	if !strings.Contains(out, "7.0,20.0 13.0,20.0") {
		t.Errorf("ширина отметки не соответствует шагу корзины:\n%s", out)
	}
	if !strings.Contains(out, `fill="url(#gradTest-`) {
		t.Errorf("нет заливки под одиночной корзиной:\n%s", out)
	}
	second := renderIsolatedForTest(t)
	if gradIDOf(out) == gradIDOf(second) {
		t.Errorf("идентификатор градиента повторился (%s) — на странице с несколькими "+
			"графиками документ невалиден, а url(#id) находит чужой градиент", gradIDOf(out))
	}

	far := []seriesPoint{
		{x: 0, y: 10, has: true},
		{x: 10, y: 50, has: false},
		{x: 20, y: 30, has: true},
	}
	var sb2 strings.Builder
	writeLineWithArea(&sb2, far, 100, "", "", `stroke="#000"`)
	if n := strings.Count(sb2.String(), "<polyline"); n != 2 {
		t.Errorf("полилиний %d, want 2 (две отдельные отметки, без соединения через пропуск)", n)
	}

	var sb3 strings.Builder
	writeLineWithArea(&sb3, []seriesPoint{{x: 0, y: 1, has: false}}, 100, "", "", `stroke="#000"`)
	if strings.Contains(sb3.String(), "<polyline") {
		t.Errorf("ряд без данных не должен давать линий: %s", sb3.String())
	}
}

func renderIsolatedForTest(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	writeLineWithArea(&sb, []seriesPoint{{x: 10, y: 20, has: true}}, 100, "#3d7bff", "gradTest", `stroke="#3d7bff"`)
	return sb.String()
}

func gradIDOf(svg string) string {
	const marker = `<linearGradient id="`
	i := strings.Index(svg, marker)
	if i < 0 {
		return ""
	}
	rest := svg[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
