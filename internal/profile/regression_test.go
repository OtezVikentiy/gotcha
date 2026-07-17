package profile

import "testing"

func TestDecide(t *testing.T) {
	cfg := DefaultProfileRegressionConfig() // Threshold 0.5, Recovery 0.2, MinSamples 100, ShareFloor 0.05
	cases := []struct {
		name          string
		base, recent  float64
		samples       uint64
		open          bool
		want          DecisionKind
	}{
		{"open on +60% above base", 0.10, 0.16, 200, false, DecisionOpen},
		{"no open below floor", 0.02, 0.04, 200, false, DecisionNone},
		{"no open with few samples", 0.10, 0.30, 50, false, DecisionNone},
		{"no open when base zero", 0.0, 0.30, 200, false, DecisionNone},
		{"no open when barely above (< threshold)", 0.10, 0.12, 200, false, DecisionNone},
		{"bump while still breached", 0.10, 0.30, 200, true, DecisionBump},
		{"resolve on recovery", 0.10, 0.11, 200, true, DecisionResolve}, // within +20%
		{"bump in dead zone", 0.10, 0.14, 200, true, DecisionBump},      // >+20%, <+50%
		// База усохла до нуля, пока инцидент открыт (функция перестала попадать в
		// базовое окно). Инцидент обязан уметь закрыться, когда доля вернулась к
		// шумовому полу, иначе он «завис» бы в вечном Bump.
		{"resolve when base collapsed and function died", 0.0, 0.0, 200, true, DecisionResolve},
		{"resolve when base zero and share back to floor", 0.0, 0.05, 200, true, DecisionResolve}, // ==ShareFloor
		{"bump when base zero but still hot", 0.0, 0.30, 200, true, DecisionBump},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.base, c.recent, c.samples, cfg, c.open).Kind; got != c.want {
				t.Fatalf("Decide(base=%v,recent=%v,samples=%d,open=%v) = %v, want %v",
					c.base, c.recent, c.samples, c.open, got, c.want)
			}
		})
	}
}
