package profile

import "testing"

func TestDecide(t *testing.T) {
	cfg := DefaultProfileRegressionConfig()
	cases := []struct {
		name                 string
		base, recent         float64
		baseSamples, samples uint64
		open                 bool
		want                 DecisionKind
	}{
		{"open on +60% above base", 0.10, 0.16, 200, 200, false, DecisionOpen},
		{"no open below floor", 0.02, 0.04, 200, 200, false, DecisionNone},
		{"no open with few samples", 0.10, 0.30, 200, 50, false, DecisionNone},
		{"no open when base zero", 0.0, 0.30, 200, 200, false, DecisionNone},
		{"no open when barely above (< threshold)", 0.10, 0.12, 200, 200, false, DecisionNone},
		{"bump while still breached", 0.10, 0.30, 200, 200, true, DecisionBump},
		{"resolve on recovery", 0.10, 0.11, 200, 200, true, DecisionResolve},
		{"bump in dead zone", 0.10, 0.14, 200, 200, true, DecisionBump},
		{"resolve when base collapsed and function died", 0.0, 0.0, 200, 200, true, DecisionResolve},
		{"resolve when base zero and share back to floor", 0.0, 0.05, 200, 200, true, DecisionResolve},
		{"bump when base zero but still hot", 0.0, 0.30, 200, 200, true, DecisionBump},
		{"no open when base has few samples", 0.10, 0.30, 50, 200, false, DecisionNone},
		{"no open when base has no samples at all", 0.10, 0.30, 0, 200, false, DecisionNone},
		{"bump despite thin base", 0.10, 0.30, 50, 200, true, DecisionBump},
		{"resolve despite thin base", 0.10, 0.11, 50, 200, true, DecisionResolve},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.base, c.recent, c.baseSamples, c.samples, cfg, c.open).Kind; got != c.want {
				t.Fatalf("Decide(base=%v,recent=%v,baseSamples=%d,samples=%d,open=%v) = %v, want %v",
					c.base, c.recent, c.baseSamples, c.samples, c.open, got, c.want)
			}
		})
	}
}
