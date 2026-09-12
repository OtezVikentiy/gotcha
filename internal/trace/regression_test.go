package trace_test

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestDecide(t *testing.T) {
	cfg := trace.DefaultRegressionConfig()
	cfg.ThresholdPct = 0.25
	cfg.RecoveryPct = 0.10
	cfg.MinSamples = 100
	cfg.DurationFloorMs = 100

	const md = "duration"

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
		{"open both conditions", s(800, 200), s(1100, 200), false, "open"},
		{"below threshold", s(800, 200), s(900, 200), false, "none"},
		{"open floor not binding", s(800, 200), s(1050, 200), false, "open"},
		{"floor blocks small base", s(40, 200), s(80, 200), false, "none"},
		{"resolve under recovery", s(800, 200), s(860, 200), true, "resolve"},
		{"resolve at recovery boundary", s(800, 200), s(880, 200), true, "resolve"},
		{"stay open above recovery", s(800, 200), s(900, 200), true, "none"},
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

func TestDecideFloorRejectsSmallAbsoluteGrowth(t *testing.T) {
	cfg := trace.DefaultRegressionConfig()
	base := trace.RegressionSample{Value: 20, Samples: 1000}
	recent := trace.RegressionSample{Value: 40, Samples: 1000}

	got := trace.Decide(base, recent, cfg, "duration", false)
	if got.Kind == trace.DecisionOpen {
		t.Fatalf("рост 20→40 мс открыл регрессию: абсолютный пол %v мс не сработал",
			cfg.DurationFloorMs)
	}
}

func TestDecideFloorAllowsRealGrowth(t *testing.T) {
	cfg := trace.DefaultRegressionConfig()
	base := trace.RegressionSample{Value: 400, Samples: 1000}
	recent := trace.RegressionSample{Value: 900, Samples: 1000}

	got := trace.Decide(base, recent, cfg, "duration", false)
	if got.Kind != trace.DecisionOpen {
		t.Fatalf("рост 400→900 мс не открыл регрессию: Kind = %v", got.Kind)
	}
}
