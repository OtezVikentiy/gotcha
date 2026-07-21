package web

import (
	"math"
	"strconv"
	"testing"
)

// TestCoverSVGHelpers покрывает краевые ветки чистых svg-хелперов, которые в
// обычных графиках не встречаются (пустые/нулевые/нефинитные данные).
func TestCoverSVGHelpers(t *testing.T) {
	// truncateRunes: n<=0 → "", строка короче n → как есть, иначе обрезка.
	if truncateRunes("hello", 0) != "" {
		t.Error("truncateRunes n<=0")
	}
	if truncateRunes("hi", 5) != "hi" {
		t.Error("truncateRunes short")
	}
	if truncateRunes("hello", 2) != "he" {
		t.Error("truncateRunes cut")
	}

	// formatAxisValue: миллионы (M), тысячи (k), обычные; юнит "1" не печатается.
	for _, tc := range []struct {
		v    float64
		unit string
	}{{2e6, ""}, {5000, "ms"}, {42, "1"}, {-3e6, ""}} {
		if formatAxisValue(tc.v, tc.unit) == "" {
			t.Errorf("formatAxisValue(%v,%q) empty", tc.v, tc.unit)
		}
	}

	// comparatorSymbol: lt → <, иначе >.
	if comparatorSymbol("lt") != "<" || comparatorSymbol("gt") != ">" {
		t.Error("comparatorSymbol branches")
	}

	// formatCoord: нефинитное → "0.0", финитное → как есть.
	if formatCoord(math.NaN()) != "0.0" || formatCoord(math.Inf(1)) != "0.0" {
		t.Error("formatCoord non-finite")
	}
	if formatCoord(1.5) != "1.5" {
		t.Error("formatCoord finite")
	}

	// niceStep: нулевой max → 1, малый raw<1 → 1, нормальный ряд 1/2/5×10ⁿ.
	if niceStep(0, 4) != 1 || niceStep(3, 10) != 1 {
		t.Error("niceStep degenerate")
	}
	if niceStep(1000, 4) == 0 {
		t.Error("niceStep normal zero")
	}
	// niceStepFloat: max<=0 → 1, нормальный.
	if niceStepFloat(0, 4) != 1 || niceStepFloat(-5, 4) != 1 {
		t.Error("niceStepFloat degenerate")
	}
	if niceStepFloat(2500, 4) <= 0 {
		t.Error("niceStepFloat normal")
	}

	// sparklinePolyline: все нули (max==0) уходит в ветку поиска lo/hi; обычный ряд.
	fmtU := func(v uint64) string { return strconv.FormatUint(v, 10) }
	if sparklinePolyline([]uint64{0, 0, 0}, 40, 12, fmtU) == "" {
		t.Error("sparklinePolyline all-zero empty")
	}
	if sparklinePolyline([]uint64{1, 5, 3}, 40, 12, fmtU) == "" {
		t.Error("sparklinePolyline normal empty")
	}
}
