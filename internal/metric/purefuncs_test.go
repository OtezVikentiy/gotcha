package metric

import "testing"

// TestScalarAggExpr — SQL-выражение агрегации по типу метрики: у histogram
// не-перцентиль даёт среднее наблюдение, у прочих — выбранную функцию, а
// неизвестная агрегация падает в avg.
func TestScalarAggExpr(t *testing.T) {
	cases := []struct{ typ, agg, want string }{
		{"histogram", "avg", "if(sum(count) = 0, 0, sum(value) / sum(count))"},
		{"gauge", "max", "max(value)"},
		{"gauge", "min", "min(value)"},
		{"gauge", "sum", "sum(value)"},
		{"gauge", "last", "argMax(value, ts)"},
		{"gauge", "", "avg(value)"},
		{"counter", "unknown", "avg(value)"},
	}
	for _, c := range cases {
		if got := scalarAggExpr(c.typ, c.agg); got != c.want {
			t.Errorf("scalarAggExpr(%q,%q) = %q, want %q", c.typ, c.agg, got, c.want)
		}
	}
}

// TestPercentileValue / TestIsPercentile — перцентильные агрегации.
func TestPercentileValue(t *testing.T) {
	for agg, want := range map[string]float64{"p50": 0.5, "p95": 0.95, "p99": 0.99, "avg": 0.5} {
		if got := percentileValue(agg); got != want {
			t.Errorf("percentileValue(%q) = %v, want %v", agg, got, want)
		}
	}
	for _, p := range []string{"p50", "p95", "p99"} {
		if !isPercentile(p) {
			t.Errorf("isPercentile(%q) = false", p)
		}
	}
	if isPercentile("avg") {
		t.Error("isPercentile(avg) = true")
	}
}

// TestWorse — «хуже» зависит от направления сравнения: для lt меньшее хуже,
// иначе большее.
func TestWorse(t *testing.T) {
	if worse("lt", 10, 5) != 5 {
		t.Error("lt: 5 хуже 10")
	}
	if worse("lt", 5, 10) != 5 {
		t.Error("lt: 5 остаётся худшим")
	}
	if worse("gt", 5, 10) != 10 {
		t.Error("gt: 10 хуже 5")
	}
	if worse("gt", 10, 5) != 10 {
		t.Error("gt: 10 остаётся худшим")
	}
}

// TestMatchersClause — пустой срез матчеров не добавляет ни SQL, ни
// аргументов; N матчеров дают N AND-условий и 2N аргументов.
func TestMatchersClause(t *testing.T) {
	if matchersClause(nil) != "" {
		t.Error("пустой срез матчеров должен давать пустой clause")
	}
	if matchersClause([]LabelMatcher{{Key: "host"}}) == "" {
		t.Error("непустой матчер должен давать clause")
	}
	if got := matchersClause([]LabelMatcher{{Key: "state"}, {Key: "cpu"}}); got != " AND attributes[?] = ? AND attributes[?] = ?" {
		t.Errorf("два матчера должны дать два AND-условия, got %q", got)
	}
	if len(appendMatchersArgs(nil, nil)) != 0 {
		t.Error("пустой срез матчеров не должен добавлять args")
	}
	if got := appendMatchersArgs(nil, []LabelMatcher{{Key: "host", Value: "api-1"}}); len(got) != 2 {
		t.Errorf("один матчер должен добавить 2 арга, got %v", got)
	}
	if got := appendMatchersArgs(nil, []LabelMatcher{{Key: "state", Value: "used"}, {Key: "cpu", Value: "0"}}); len(got) != 4 {
		t.Errorf("два матчера должны добавить 4 арга, got %v", got)
	}
}

// TestCompactMatchers — пустой Key отфильтровывается, непустой остаётся.
func TestCompactMatchers(t *testing.T) {
	if got := compactMatchers(nil); len(got) != 0 {
		t.Errorf("compactMatchers(nil) = %v, want пусто", got)
	}
	got := compactMatchers([]LabelMatcher{{Key: "", Value: "x"}, {Key: "host", Value: "api-1"}, {}})
	if len(got) != 1 || got[0].Key != "host" {
		t.Errorf("compactMatchers = %+v, want один матчер host", got)
	}
}
