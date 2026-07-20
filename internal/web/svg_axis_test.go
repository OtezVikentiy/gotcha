package web

import "testing"

// TestFormatAxisValueSkipsDimensionlessUnit — по соглашению OTLP безразмерная
// метрика несёт юнит "1". Печатать его на оси нельзя: подпись «17 1»
// читается как число «17 1», а не как «17 штук».
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
	}
	for _, c := range cases {
		if got := formatAxisValue(c.v, c.unit); got != c.want {
			t.Errorf("formatAxisValue(%v, %q) = %q, want %q", c.v, c.unit, got, c.want)
		}
	}
}
