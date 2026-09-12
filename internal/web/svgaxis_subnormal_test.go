package web

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
)

// Тесты этого файла гонят проверку в отдельной горутине с дедлайном: регрессия на
// субнормали или переполнении оси — бесконечный цикл, который иначе подвесит весь прогон.
func runWithDeadline(t *testing.T, timeout time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("не завершилось за %s — похоже на зависший цикл по шагу оси", timeout)
	}
}

func TestNiceStepFloatSubnormalGivesPositiveFiniteStep(t *testing.T) {
	for _, max := range []float64{
		4.9406564584124654e-324, // math.SmallestNonzeroFloat64
		1.5e-323, 1e-320, 1e-310, 1e-300,
		0, -5,
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		step := niceStepFloat(max, 3)
		if step <= 0 || math.IsNaN(step) || math.IsInf(step, 0) {
			t.Errorf("niceStepFloat(%v, 3) = %v, want strictly positive finite step", max, step)
		}
	}
}

func TestNewYScaleFloatSubnormalTerminates(t *testing.T) {
	for _, max := range []float64{
		4.9406564584124654e-324, 1.5e-323, 1e-320, 1e-310, 1e-300,
		math.Inf(1), math.NaN(),
	} {
		max := max
		runWithDeadline(t, 2*time.Second, func() {
			s := newYScaleFloat(max, 3)
			if s.step <= 0 || math.IsNaN(s.step) || math.IsInf(s.step, 0) {
				t.Errorf("newYScaleFloat(%v): step = %v, want strictly positive finite", max, s.step)
			}
			if !math.IsNaN(max) && !math.IsInf(max, 0) && max > 0 && s.top <= max {
				t.Errorf("newYScaleFloat(%v): top = %v, want top > max", max, s.top)
			}
		})
	}
}

// Защита в yAxisTicks обязана держаться сама по себе, а не только через
// newYScaleFloat: yScale сюда может прийти собранной вручную, с ощутимым top и
// денормализованным шагом.
func TestYAxisTicksCapsIterationsOnDegenerateScale(t *testing.T) {
	runWithDeadline(t, 2*time.Second, func() {
		ticks := yAxisTicks(yScale{top: 1000, step: 4.9406564584124654e-324})
		if len(ticks) == 0 || len(ticks) > maxAxisScaleSteps {
			t.Errorf("yAxisTicks: len = %d, want 1..%d", len(ticks), maxAxisScaleSteps)
		}
	})
}

func TestFitYLabelsTerminatesOnDegenerateScale(t *testing.T) {
	g := newChartGeom(720, 200, 40, 10, 10, 22)
	runWithDeadline(t, 2*time.Second, func() {
		g.fitYLabels(yScale{top: 1000, step: 4.9406564584124654e-324}, func(v float64) string { return "x" })
	})
}

func TestWriteYGridTerminatesOnDegenerateScale(t *testing.T) {
	g := newChartGeom(720, 200, 40, 10, 10, 22)
	var sb strings.Builder
	runWithDeadline(t, 2*time.Second, func() {
		writeYGrid(&sb, g, yScale{top: 1000, step: 4.9406564584124654e-324}, func(v float64) string { return "x" })
	})
}

func TestMultiSeriesMarkupSubnormalPointTerminates(t *testing.T) {
	points := []metric.Point{
		{T: time.Now(), V: 4.9406564584124654e-324},
		{T: time.Now().Add(time.Minute), V: 4.9406564584124654e-324},
	}
	series := []NamedSeries{{Label: "s", Points: points}}
	runWithDeadline(t, 2*time.Second, func() {
		out := multiSeriesMarkup(context.Background(), series, "", nil, nil, 720, 200)
		if out == "" {
			t.Error("multiSeriesMarkup: пустой результат")
		}
	})
}

func TestNewYScaleFloatOverflowNearMaxFloat64Terminates(t *testing.T) {
	for _, max := range []float64{math.MaxFloat64, 1.7e308, 1e308, 1e300} {
		max := max
		runWithDeadline(t, 2*time.Second, func() {
			s := newYScaleFloat(max, 3)
			if s.step <= 0 || math.IsNaN(s.step) || math.IsInf(s.step, 0) {
				t.Errorf("newYScaleFloat(%v): step = %v, want strictly positive finite", max, s.step)
			}
			if math.IsNaN(s.top) || math.IsInf(s.top, 0) {
				t.Errorf("newYScaleFloat(%v): top = %v, want finite", max, s.top)
			}
			if s.top < max {
				t.Errorf("newYScaleFloat(%v): top = %v, want top >= max", max, s.top)
			}
		})
	}
	// ниже зоны переполнения запас над максимумом обязан остаться строгим, как раньше.
	if s := newYScaleFloat(1e18, 3); s.top <= 1e18 {
		t.Errorf("newYScaleFloat(1e18): top = %v, want top > max", s.top)
	}
}

func TestYAxisTicksNeverEmitsNonFiniteTick(t *testing.T) {
	for _, s := range []yScale{
		{top: math.MaxFloat64, step: 1e308},
		{top: 1.7e308, step: 1e308},
		{top: math.Inf(1), step: 1e18},
	} {
		s := s
		runWithDeadline(t, 2*time.Second, func() {
			for _, v := range yAxisTicks(s) {
				if math.IsInf(v, 0) || math.IsNaN(v) {
					t.Errorf("yAxisTicks(%+v): неконечный тик %v", s, v)
				}
			}
		})
	}
}

// Прямая проверка, что при переполнении шкала не лжёт: точки на разных величинах обязаны
// лечь на разные y, а не все на нулевую линию (yFor(v/+Inf)=0 для любого v).
func TestYForOverflowScaleDoesNotFlattenPoints(t *testing.T) {
	g := newChartGeom(720, 200, 58, 16, 12, 26)
	for _, max := range []float64{math.MaxFloat64, 1.7e308, 1e308} {
		s := newYScaleFloat(max, 3)
		yHalf := s.yFor(g, max/2)
		yFull := s.yFor(g, max)
		if yHalf == yFull {
			t.Errorf("newYScaleFloat(%v): yFor(max/2)=%v == yFor(max)=%v, шкала выродилась в плоскую линию", max, yHalf, yFull)
		}
	}
}

func TestMultiSeriesMarkupHugeValueTerminatesAndStaysFinite(t *testing.T) {
	for _, v := range []float64{math.MaxFloat64, 1.7e308, 1e308, 1e300, 1e18} {
		v := v
		points := []metric.Point{
			{T: time.Now(), V: v / 2},
			{T: time.Now().Add(time.Minute), V: v},
		}
		series := []NamedSeries{{Label: "s", Points: points}}
		runWithDeadline(t, 2*time.Second, func() {
			out := multiSeriesMarkup(context.Background(), series, "", nil, nil, 720, 200)
			if strings.Contains(out, "Inf") || strings.Contains(out, "NaN") {
				t.Errorf("v=%v: в SVG неконечная подпись/значение: %s", v, out)
			}
		})
	}
}
