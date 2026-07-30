package templates

import "testing"

// TestRegressionValueUnits: единица зависит от метрики.
//
// p95 длительности приходит из transactions_5m в МИКРОсекундах, web-vital'ы — в
// миллисекундах. Общий формат «как у vitals» показывал «640.00s» там, где на
// самом деле 640 мс: значения длительности были завышены в тысячу раз, и
// «регрессия p95 до 19 минут» выглядела правдоподобно ровно настолько, чтобы
// никто не проверил.
func TestRegressionValueUnits(t *testing.T) {
	cases := []struct {
		metric string
		value  float64
		want   string
	}{
		{"duration", 640000, "640.0ms"}, // 640 000 мкс = 640 мс
		{"duration", 1180000, "1.18s"},  // 1 180 000 мкс = 1.18 с
		{"duration", 900, "900µs"},      // меньше миллисекунды
		{"lcp", 2100, "2.10s"},          // 2100 мс = 2.1 с
		{"inp", 371, "371ms"},           // миллисекунды как есть
		{"cls", 0.18, "0.18"},           // безразмерный
	}
	for _, tc := range cases {
		if got := formatRegressionValue(tc.metric, tc.value); got != tc.want {
			t.Errorf("formatRegressionValue(%q, %v) = %q, want %q", tc.metric, tc.value, got, tc.want)
		}
	}
}
