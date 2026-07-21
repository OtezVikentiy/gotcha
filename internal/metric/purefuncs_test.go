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

// TestMatcherClause — пустой ключ фильтра не добавляет ни SQL, ни аргументов.
func TestMatcherClause(t *testing.T) {
	if matcherClause(LabelMatcher{}) != "" {
		t.Error("пустой matcher должен давать пустой clause")
	}
	if matcherClause(LabelMatcher{Key: "host"}) == "" {
		t.Error("непустой matcher должен давать clause")
	}
	if len(appendMatcherArgs(nil, LabelMatcher{})) != 0 {
		t.Error("пустой matcher не должен добавлять args")
	}
	if got := appendMatcherArgs(nil, LabelMatcher{Key: "host", Value: "api-1"}); len(got) != 2 {
		t.Errorf("непустой matcher должен добавить 2 арга, got %v", got)
	}
}
