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
		// Значения от миллиарда: суффиксы G/T, а не научная нотация
		// («1e+03M») — тот же дефект-класс, что QA-находка «avg > 8e+08».
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
