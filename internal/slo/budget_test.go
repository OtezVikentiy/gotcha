package slo_test

import (
	"math"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
)

const eps = 1e-9

// TestAttainment — доля хороших событий; ok=false при пустом окне.
func TestAttainment(t *testing.T) {
	// 995/1000 → 0.995.
	bs := []slo.Bucket{{Good: 995, Total: 1000}}
	if a, ok := slo.Attainment(bs); !ok || math.Abs(a-0.995) > eps {
		t.Fatalf("Attainment = %v ok=%v, want 0.995 true", a, ok)
	}
	// сумма по нескольким корзинам: (90+95)/(100+100)=0.925.
	multi := []slo.Bucket{{Good: 90, Total: 100}, {Good: 95, Total: 100}}
	if a, ok := slo.Attainment(multi); !ok || math.Abs(a-0.925) > eps {
		t.Fatalf("Attainment multi = %v ok=%v, want 0.925 true", a, ok)
	}
	// пустое окно (nil) → ok=false, ratio=0.
	if a, ok := slo.Attainment(nil); ok || a != 0 {
		t.Fatalf("Attainment(nil) = %v ok=%v, want 0 false", a, ok)
	}
	// корзины есть, но все с Total=0 → ok=false.
	if a, ok := slo.Attainment([]slo.Bucket{{Good: 0, Total: 0}, {Good: 0, Total: 0}}); ok || a != 0 {
		t.Fatalf("Attainment(total=0) = %v ok=%v, want 0 false", a, ok)
	}
	// идеальное окно → 1.0.
	if a, ok := slo.Attainment([]slo.Bucket{{Good: 50, Total: 50}}); !ok || math.Abs(a-1.0) > eps {
		t.Fatalf("Attainment(perfect) = %v ok=%v, want 1 true", a, ok)
	}
}

// TestBudgetFractions — потребление и остаток бюджета.
func TestBudgetFractions(t *testing.T) {
	// target 0.99 → бюджет 1%. attainment 0.995 → badRate 0.005 → consumed 0.5.
	bs := []slo.Bucket{{Good: 995, Total: 1000}}
	if c, ok := slo.BudgetConsumedFraction(bs, 0.99); !ok || math.Abs(c-0.5) > eps {
		t.Fatalf("consumed = %v ok=%v, want 0.5 true", c, ok)
	}
	if r, ok := slo.BudgetRemainingFraction(bs, 0.99); !ok || math.Abs(r-0.5) > eps {
		t.Fatalf("remaining = %v ok=%v, want 0.5 true", r, ok)
	}
	// бюджет исчерпан ровно: attainment == target → consumed 1, remaining 0.
	exact := []slo.Bucket{{Good: 990, Total: 1000}}
	if c, _ := slo.BudgetConsumedFraction(exact, 0.99); math.Abs(c-1.0) > eps {
		t.Fatalf("consumed(exact) = %v, want 1", c)
	}
	if r, _ := slo.BudgetRemainingFraction(exact, 0.99); math.Abs(r-0.0) > eps {
		t.Fatalf("remaining(exact) = %v, want 0", r)
	}
	// перерасход: attainment ниже target → consumed>1, remaining<0.
	over := []slo.Bucket{{Good: 980, Total: 1000}} // badRate 0.02 → consumed 2.0
	if c, _ := slo.BudgetConsumedFraction(over, 0.99); math.Abs(c-2.0) > eps {
		t.Fatalf("consumed(over) = %v, want 2", c)
	}
	if r, _ := slo.BudgetRemainingFraction(over, 0.99); math.Abs(r-(-1.0)) > eps {
		t.Fatalf("remaining(over) = %v, want -1", r)
	}
	// attainment > target (плохих мало): consumed<1, remaining>0.
	good := []slo.Bucket{{Good: 999, Total: 1000}} // badRate 0.001 → consumed 0.1
	if c, _ := slo.BudgetConsumedFraction(good, 0.99); math.Abs(c-0.1) > eps {
		t.Fatalf("consumed(good) = %v, want 0.1", c)
	}
	// пустое окно → ok=false, frac=0.
	if c, ok := slo.BudgetConsumedFraction(nil, 0.99); ok || c != 0 {
		t.Fatalf("consumed(nil) = %v ok=%v, want 0 false", c, ok)
	}
	if r, ok := slo.BudgetRemainingFraction(nil, 0.99); ok || r != 0 {
		t.Fatalf("remaining(nil) = %v ok=%v, want 0 false", r, ok)
	}
	// target близко к 1 (бюджет → 0): не паника, не Inf/NaN. attainment 0.999.
	tight := []slo.Bucket{{Good: 9999, Total: 10000}}
	c, ok := slo.BudgetConsumedFraction(tight, 0.999) // badRate 0.0001 / 0.001 = 0.1
	if !ok || math.IsInf(c, 0) || math.IsNaN(c) || math.Abs(c-0.1) > eps {
		t.Fatalf("consumed(tight) = %v ok=%v, want finite 0.1", c, ok)
	}
}

// TestBurnRate — скорость сжигания на переданном под-окне.
func TestBurnRate(t *testing.T) {
	// badRate 0.005 / (1-0.99)=0.01 → 0.5.
	bs := []slo.Bucket{{Good: 995, Total: 1000}}
	if br, ok := slo.BurnRate(bs, 0.99); !ok || math.Abs(br-0.5) > eps {
		t.Fatalf("BurnRate = %v ok=%v, want 0.5 true", br, ok)
	}
	// сильный прожог: badRate 0.2 / 0.01 → 20.
	hot := []slo.Bucket{{Good: 800, Total: 1000}}
	if br, ok := slo.BurnRate(hot, 0.99); !ok || math.Abs(br-20.0) > eps {
		t.Fatalf("BurnRate(hot) = %v ok=%v, want 20 true", br, ok)
	}
	// attainment > target (мало плохих) → burn < 1.
	cool := []slo.Bucket{{Good: 999, Total: 1000}} // badRate 0.001 / 0.01 → 0.1
	if br, ok := slo.BurnRate(cool, 0.99); !ok || math.Abs(br-0.1) > eps {
		t.Fatalf("BurnRate(cool) = %v ok=%v, want 0.1 true", br, ok)
	}
	// идеальное окно → burn 0.
	if br, ok := slo.BurnRate([]slo.Bucket{{Good: 100, Total: 100}}, 0.99); !ok || br != 0 {
		t.Fatalf("BurnRate(perfect) = %v ok=%v, want 0 true", br, ok)
	}
	// пустое окно → ok=false, rate=0.
	if br, ok := slo.BurnRate(nil, 0.99); ok || br != 0 {
		t.Fatalf("BurnRate(nil) = %v ok=%v, want 0 false", br, ok)
	}
	if br, ok := slo.BurnRate([]slo.Bucket{{Total: 0}}, 0.99); ok || br != 0 {
		t.Fatalf("BurnRate(total=0) = %v ok=%v, want 0 false", br, ok)
	}
}

// TestDecideBurn — двухоконное решение открыть/закрыть.
func TestDecideBurn(t *testing.T) {
	target, thr := 0.99, 14.4
	hot := []slo.Bucket{{Good: 800, Total: 1000}}   // burn 20
	cool := []slo.Bucket{{Good: 1000, Total: 1000}} // burn 0

	// оба окна горят 20× ≥ 14.4 → open, не close.
	d := slo.DecideBurn(hot, hot, target, thr)
	if !d.OpenSignal || d.CloseSignal {
		t.Fatalf("оба горят → open !close, got %+v", d)
	}
	if math.Abs(d.BurnLong-20) > eps || math.Abs(d.BurnShort-20) > eps {
		t.Fatalf("burn long/short = %v/%v, want 20/20", d.BurnLong, d.BurnShort)
	}

	// длинное горит, короткое остыло → не open, close.
	d2 := slo.DecideBurn(hot, cool, target, thr)
	if d2.OpenSignal || !d2.CloseSignal {
		t.Fatalf("короткое остыло → !open close, got %+v", d2)
	}
	if math.Abs(d2.BurnShort-0) > eps {
		t.Fatalf("BurnShort остывшего = %v, want 0", d2.BurnShort)
	}

	// короткое горит, длинное остыло → не open (нужны оба), не close (короткое горит).
	d3 := slo.DecideBurn(cool, hot, target, thr)
	if d3.OpenSignal || d3.CloseSignal {
		t.Fatalf("длинное остыло, короткое горит → !open !close, got %+v", d3)
	}

	// граница инклюзивна (≥): порог == фактический burn → open.
	// Порог берём из самой формулы, чтобы избежать FP-неточности равенства.
	onThr := []slo.Bucket{{Good: 856, Total: 1000}}
	exactBurn, _ := slo.BurnRate(onThr, target)
	dThr := slo.DecideBurn(onThr, onThr, target, exactBurn)
	if !dThr.OpenSignal {
		t.Fatalf("burn ровно на пороге → open (граница ≥), got %+v", dThr)
	}

	// пустое короткое окно (нет данных) → burn 0 → не open; close (burnShort<thr).
	dEmpty := slo.DecideBurn(hot, nil, target, thr)
	if dEmpty.OpenSignal || !dEmpty.CloseSignal {
		t.Fatalf("пустое короткое → !open close, got %+v", dEmpty)
	}
	if dEmpty.BurnShort != 0 {
		t.Fatalf("BurnShort пустого = %v, want 0", dEmpty.BurnShort)
	}
}
