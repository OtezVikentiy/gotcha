package templates

import "testing"

// TestProfileWeightText — вес выборок приводится к единице своего типа
// профиля. Голое число вводило в заблуждение: «284000000» в колонке с
// заголовком «Замеры» читалось как 284 миллиона замеров, хотя это 284 мс
// процессорного времени.
func TestProfileWeightText(t *testing.T) {
	cases := []struct {
		typ    string
		unit   string
		weight uint64
		want   string
	}{
		// Единица приходит из данных (pprof SampleType.Unit).
		{"cpu", "nanoseconds", 284_000_000, "284ms"},
		{"cpu", "nanoseconds", 20_206_000_000, "20.21s"},
		{"cpu", "nanoseconds", 5_000, "5µs"},
		{"alloc_space", "bytes", 12 * 1024 * 1024, "12.0MB"},
		{"samples", "count", 4200, "4200"},
		{"wall", "milliseconds", 1_500, "1.50s"},
		{"wall", "milliseconds", 2, "2ms"},
		// Единица известна источнику, но не нам — показываем как есть.
		{"custom", "requests", 42, "42 requests"},
		// Строки до миграции 0012: единицы нет, возвращаемся к догадке по типу.
		{"cpu", "", 284_000_000, "284ms"},
		{"heap", "", 900, "900B"},
		{"custom", "", 42, "42"},
	}
	for _, c := range cases {
		if got := profileWeightText(c.typ, c.unit, c.weight); got != c.want {
			t.Errorf("profileWeightText(%q, %q, %d) = %q, want %q", c.typ, c.unit, c.weight, got, c.want)
		}
	}
}
