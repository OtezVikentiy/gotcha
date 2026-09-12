package web

import (
	"testing"
	"time"
)

// по соглашению OTLP безразмерная метрика несёт юнит "1" — печатать его на
// оси нельзя, подпись «17 1» читается как число, а не как «17 штук».
func TestFormatAxisValueSkipsDimensionlessUnit(t *testing.T) {
	cases := []struct {
		v    float64
		unit string
		want string
	}{
		{17, "1", "17"},
		{10.5, "1", "10.5"},
		{17, "", "17"},
		{17, "ms", "17 ms"},
		{1500, "ms", "1.5k ms"},
		// от миллиарда — суффиксы G/T, не научная нотация.
		{9.1e8, "By", "910M By"},
		{1.25e9, "", "1.25G"},
		{5e12, "", "5T"},
	}
	for _, c := range cases {
		if got := formatAxisValue(c.v, c.unit); got != c.want {
			t.Errorf("formatAxisValue(%v, %q) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
}

func TestMetricTimeLabelUsesUTC(t *testing.T) {
	loc := time.FixedZone("UTC+5", 5*60*60)
	// 23:30 в UTC+5 — это 18:30 UTC того же дня.
	t1 := time.Date(2026, 8, 12, 23, 30, 0, 0, loc)
	if got, want := metricTimeLabel(t1, 24), "18:30"; got != want {
		t.Errorf("metricTimeLabel(%v, 24) = %q, want %q (UTC-normalized)", t1, got, want)
	}

	t2 := time.Date(2026, 8, 12, 23, 30, 0, 0, loc) // 18:30 UTC 12 августа
	if got, want := metricTimeLabel(t2, 48), "12.08"; got != want {
		t.Errorf("metricTimeLabel(%v, 48) = %q, want %q (UTC-normalized)", t2, got, want)
	}
}
