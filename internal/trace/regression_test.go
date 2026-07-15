package trace_test

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestDecide проверяет чистую логику §6 таблицей: относительный порог И
// абсолютный пол на открытие, гистерезис на закрытие, отсечка по сэмплам и
// отсутствию базы. Docker не нужен, под -short не скипается.
func TestDecide(t *testing.T) {
	// Конфиг из дефолтов, но с явными значениями из брифа: threshold 0.25,
	// recovery 0.10, min_samples 100, duration floor 100.
	cfg := trace.DefaultRegressionConfig()
	cfg.ThresholdPct = 0.25
	cfg.RecoveryPct = 0.10
	cfg.MinSamples = 100
	cfg.DurationFloorMs = 100

	const md = "duration" // метрика с полом 100

	s := func(v float64, n int) trace.RegressionSample {
		return trace.RegressionSample{Value: v, Samples: n}
	}

	cases := []struct {
		name   string
		base   trace.RegressionSample
		recent trace.RegressionSample
		open   bool
		want   string
	}{
		// База 800, порог даёт 1000, пол даёт 900.
		{"open both conditions", s(800, 200), s(1100, 200), false, "open"},   // 1100 > 1000 и > 900
		{"below threshold", s(800, 200), s(900, 200), false, "none"},         // +12.5% < 25%
		{"open floor not binding", s(800, 200), s(1050, 200), false, "open"}, // 1050 > 1000 и > 900
		{"floor blocks small base", s(40, 200), s(80, 200), false, "none"},   // +100% но 80 < 40+100=140
		// Открытый инцидент: recovery-порог 800×1.1 = 880.
		{"resolve under recovery", s(800, 200), s(860, 200), true, "resolve"}, // 860 ≤ 880
		{"resolve at recovery boundary", s(800, 200), s(880, 200), true, "resolve"}, // 880 == 880, граница включительна
		{"stay open above recovery", s(800, 200), s(900, 200), true, "none"},  // 900 > 880
		// Отсечки.
		{"low recent samples", s(800, 200), s(1100, 50), false, "none"},
		{"low base samples", s(800, 50), s(1100, 200), false, "none"},
		{"no baseline", s(0, 200), s(1100, 200), false, "none"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := trace.Decide(c.base, c.recent, cfg, md, c.open)
			if got.Kind != c.want {
				t.Fatalf("Decide(base=%+v, recent=%+v, open=%v) = %q, want %q",
					c.base, c.recent, c.open, got.Kind, c.want)
			}
		})
	}
}
