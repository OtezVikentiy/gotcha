package slo

import (
	"testing"
	"time"
)

// TestSLOEvaluatorTickBudget — табличный тест на tickBudget() (K15-1): доля
// Interval (tickBudgetShare), но не меньше пола (minTickBudget). Белый ящик —
// tickBudget неэкспортирован, поэтому файл живёт в package slo, а не slo_test
// (как остальные тесты пакета, гоняющие Evaluator через реальные PG/CH).
func TestSLOEvaluatorTickBudget(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"короткий интервал упирается в пол", time.Second, minTickBudget},
		{"интервал ровно на полу", minTickBudget, minTickBudget},
		{"минута сверх пола считается долей", 60 * time.Second, 48 * time.Second},
		{"не задан — дефолт SLO-интервала (2 минуты) даёт 96s", 0, 96 * time.Second},
		{"отрицательный трактуется как не заданный", -time.Second, 96 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &Evaluator{Interval: tc.interval}
			if got := e.tickBudget(); got != tc.want {
				t.Errorf("tickBudget() с Interval=%v = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}
