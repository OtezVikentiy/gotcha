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

// TestDecideEqualToThresholdIsNotABreach — K3-6: граница строгая. Значение,
// равное порогу, нарушением не считается ни для gt, ни для lt (валидация
// правил допускает порог, совпадающий с потолком метрики, — «1.0» для доли;
// равенство ему не открывает инцидент). Для открытого инцидента равенство
// порогу — мёртвая зона: не закрываем, держим.
func TestDecideEqualToThresholdIsNotABreach(t *testing.T) {
	for _, cmp := range []string{"gt", "lt"} {
		if got := Decide(100, cmp, 100, false); got != (Decision{}) {
			t.Errorf("%s: current == threshold opened an incident: %+v", cmp, got)
		}
		if got := Decide(100, cmp, 100, true); got != (Decision{Bump: true}) {
			t.Errorf("%s: current == threshold with open incident = %+v, want Bump (dead zone)", cmp, got)
		}
	}
	if got := Decide(1, "gt", 1, false); got != (Decision{}) {
		t.Errorf("gt: 1 == 1 opened an incident: %+v", got)
	}
}

// TestDecideNegativeThresholdHysteresis — полоса гистерезиса берётся от модуля
// порога. Умножение на (1±band) разворачивало её на отрицательных порогах:
// при пороге -100 «безопасной стороной» для gt оказывалось -95, то есть выше
// порога, и значение -98 было одновременно нарушением и восстановлением. Decide
// открывал инцидент, следующим тиком закрывал, и так каждую минуту.
func TestDecideNegativeThresholdHysteresis(t *testing.T) {
	const th = -100.0

	// Нарушение и восстановление взаимоисключающи при любом значении — это и
	// есть свойство, которое ломалось.
	for _, cur := range []float64{-120, -106, -105, -104, -100, -98, -95, -50, 0, 50} {
		for _, cmp := range []string{"gt", "lt"} {
			opened := Decide(cur, cmp, th, false)
			held := Decide(cur, cmp, th, true)
			if opened.Open && held.Close {
				t.Errorf("%s threshold=%v current=%v: одновременно нарушение и восстановление", cmp, th, cur)
			}
		}
	}

	// gt: нарушение выше -100, восстановление только ниже -105 (полоса 5% от 100).
	if d := Decide(-98, "gt", th, false); !d.Open {
		t.Error("gt: -98 при пороге -100 должно открывать инцидент")
	}
	if d := Decide(-98, "gt", th, true); !d.Bump {
		t.Error("gt: -98 при открытом инциденте должно держать его открытым")
	}
	if d := Decide(-104, "gt", th, true); !d.Bump {
		t.Error("gt: -104 внутри полосы — инцидент ещё не закрывается")
	}
	if d := Decide(-106, "gt", th, true); !d.Close {
		t.Error("gt: -106 вышло за полосу — инцидент закрывается")
	}

	// lt: зеркально — нарушение ниже -100, восстановление выше -95.
	if d := Decide(-102, "lt", th, false); !d.Open {
		t.Error("lt: -102 при пороге -100 должно открывать инцидент")
	}
	if d := Decide(-96, "lt", th, true); !d.Bump {
		t.Error("lt: -96 внутри полосы — инцидент ещё не закрывается")
	}
	if d := Decide(-94, "lt", th, true); !d.Close {
		t.Error("lt: -94 вышло за полосу — инцидент закрывается")
	}
}

// TestDecideZeroThresholdIsExclusive — при нулевом пороге полосы нет (её не от
// чего считать), но нарушение и восстановление обязаны оставаться
// взаимоисключающими: «порог 0» значит «любое ненулевое значение — нарушение».
func TestDecideZeroThresholdIsExclusive(t *testing.T) {
	for _, cur := range []float64{-1, -0.0001, 0, 0.0001, 1} {
		for _, cmp := range []string{"gt", "lt"} {
			opened := Decide(cur, cmp, 0, false)
			held := Decide(cur, cmp, 0, true)
			if opened.Open && held.Close {
				t.Errorf("%s threshold=0 current=%v: одновременно нарушение и восстановление", cmp, cur)
			}
		}
	}
	if d := Decide(0.5, "gt", 0, false); !d.Open {
		t.Error("gt: 0.5 при пороге 0 должно открывать инцидент")
	}
	if d := Decide(0, "gt", 0, true); !d.Close {
		t.Error("gt: 0 при пороге 0 должно закрывать инцидент")
	}
}

// TestDecidePositiveThresholdUnchanged — положительные пороги, самый частый
// случай, ведут себя ровно как прежде: полоса 5% ниже порога для gt.
func TestDecidePositiveThresholdUnchanged(t *testing.T) {
	const th = 100.0
	if d := Decide(101, "gt", th, false); !d.Open {
		t.Error("gt: 101 при пороге 100 должно открывать инцидент")
	}
	if d := Decide(96, "gt", th, true); !d.Bump {
		t.Error("gt: 96 внутри полосы — инцидент ещё не закрывается")
	}
	if d := Decide(94, "gt", th, true); !d.Close {
		t.Error("gt: 94 вышло за полосу — инцидент закрывается")
	}
}
