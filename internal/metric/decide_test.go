package metric

import "testing"

func TestDecideGT(t *testing.T) {
	cases := []struct {
		name    string
		current float64
		open    bool
		want    Decision
	}{
		{"open on breach", 150, false, Decision{Open: true}},
		{"bump while breached", 150, true, Decision{Bump: true}},
		{"hold in dead zone", 99, true, Decision{Bump: true}}, // 95..100 — не закрываем
		{"close on recovery", 90, true, Decision{Close: true}},
		{"nothing below threshold", 90, false, Decision{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Decide(c.current, "gt", 100, c.open); got != c.want {
				t.Fatalf("Decide(%v,gt,100,%v) = %+v, want %+v", c.current, c.open, got, c.want)
			}
		})
	}
}

func TestDecideLT(t *testing.T) {
	// lt threshold=100: нарушение при current<100, восстановление при current>=105.
	if got := Decide(50, "lt", 100, false); got != (Decision{Open: true}) {
		t.Fatalf("lt open = %+v", got)
	}
	if got := Decide(101, "lt", 100, true); got != (Decision{Bump: true}) {
		t.Fatalf("lt dead zone = %+v", got)
	}
	if got := Decide(110, "lt", 100, true); got != (Decision{Close: true}) {
		t.Fatalf("lt close = %+v", got)
	}
	if got := Decide(110, "lt", 100, false); got != (Decision{}) {
		t.Fatalf("lt nothing = %+v", got)
	}
}
