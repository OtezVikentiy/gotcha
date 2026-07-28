package metric

import (
	"math"
	"testing"
	"time"
)

// TestAggregatePoints — сведение ряда к одному числу. Отдельный юнит-тест без
// ClickHouse: именно здесь решается, ЧТО увидит пороговое правило, и перцентиль
// обязан быть настоящим перцентилем, а не подменяться средним.
func TestAggregatePoints(t *testing.T) {
	base := time.Unix(0, 0).UTC()
	pts := make([]Point, 0, 5)
	for i, v := range []float64{10, 2, 8, 4, 6} {
		pts = append(pts, Point{T: base.Add(time.Duration(i) * time.Minute), V: v})
	}

	cases := map[string]float64{
		"max": 10,
		"min": 2,
		"sum": 30,
		"avg": 6,
		"p50": 6,  // отсортировано: 2,4,6,8,10 → индекс 2
		"p95": 10, // ближайший ранг → последний
		"p99": 10,
	}
	for agg, want := range cases {
		if got := aggregatePoints(pts, agg); math.Abs(got-want) > 1e-9 {
			t.Errorf("aggregatePoints(%q) = %v, want %v", agg, got, want)
		}
	}

	// Неизвестная агрегация ведёт себя как avg (тот же дефолт, что и в SQL-ветке).
	if got := aggregatePoints(pts, "wat"); math.Abs(got-6) > 1e-9 {
		t.Errorf("aggregatePoints(unknown) = %v, want avg=6", got)
	}

	// Единственная точка: все агрегации дают её значение.
	one := []Point{{T: base, V: 42}}
	for _, agg := range []string{"max", "min", "sum", "avg", "p95"} {
		if got := aggregatePoints(one, agg); got != 42 {
			t.Errorf("aggregatePoints(one, %q) = %v, want 42", agg, got)
		}
	}
}

// TestScalarAggExprPercentile — перцентиль на НЕ-гистограмме должен давать
// quantile, а не avg: раньше p50/p95/p99 проваливались в default и правило с
// подписью «p95» молча сравнивало с порогом среднее.
func TestScalarAggExprPercentile(t *testing.T) {
	for _, agg := range []string{"p50", "p95", "p99"} {
		if got := scalarAggExpr("gauge", agg); got == "avg(value)" {
			t.Errorf("scalarAggExpr(gauge, %q) = %q — перцентиль подменён средним", agg, got)
		}
	}
	if got := scalarAggExpr("gauge", "avg"); got != "avg(value)" {
		t.Errorf("scalarAggExpr(gauge, avg) = %q, want avg(value)", got)
	}
	// Гистограмма считается по sum/count независимо от имени агрегации.
	if got := scalarAggExpr("histogram", "p95"); got == "avg(value)" {
		t.Errorf("scalarAggExpr(histogram, p95) = %q", got)
	}
}
