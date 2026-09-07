package templates

import (
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
)

// TestLogSavedFilterApplyURLBranches — logSavedFilterApplyURL раскладывает
// предикаты сохранённого фильтра в параметры ссылки через log.ApplyPredicates
// (устранение находки финального ревью C4: до переезда в internal/log здесь
// жила независимая копия того же switch, комментарий над которой ссылался на
// несуществующий «круговой тест», а реально покрытой оставалась лишь одна
// ветка из семи — q_not, через несвязанный сценарий фильтра по умолчанию).
// Таблица гоняет по одному предикату каждого вида — шесть положительных полей
// плюс одно отрицательное условие — и проверяет параметр, который обязана
// нести получившаяся ссылка.
func TestLogSavedFilterApplyURLBranches(t *testing.T) {
	cases := []struct {
		name      string
		predicate log.Predicate
		wantKey   string
		wantValue string
	}{
		{"severity", log.Predicate{Field: log.FieldSeverity, Op: log.OpEq, Value: "error"}, "severity", "error"},
		{"service", log.Predicate{Field: log.FieldService, Op: log.OpEq, Value: "api"}, "service", "api"},
		{"environment", log.Predicate{Field: log.FieldEnvironment, Op: log.OpEq, Value: "prod"}, "environment", "prod"},
		{"body", log.Predicate{Field: log.FieldBody, Op: log.OpContains, Value: "timeout"}, "q", "timeout"},
		{"trace_id", log.Predicate{Field: log.FieldTraceID, Op: log.OpEq, Value: "abc123"}, "trace_id", "abc123"},
		{"attr", log.Predicate{Field: log.FieldAttr, Key: "http.status", Op: log.OpEq, Value: "500"}, "attr", "http.status:500"},
		{"resource_attr", log.Predicate{Field: log.FieldResourceAttr, Key: "host.name", Op: log.OpEq, Value: "web-1"}, "attr", "res:host.name:web-1"},
		{"negative", log.Predicate{Field: log.FieldService, Op: log.OpNeq, Value: "worker"}, "service_not", "worker"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := logfilter.Filter{Predicates: []log.Predicate{tc.predicate}}
			got := logSavedFilterApplyURL(7, f)
			q := parseLogsLink(t, got, 7)
			vals := q[tc.wantKey]
			if len(vals) != 1 || vals[0] != tc.wantValue {
				t.Fatalf("logSavedFilterApplyURL(...) = %q, параметр %s = %v, want [%q]", got, tc.wantKey, vals, tc.wantValue)
			}
		})
	}
}

// TestLogSavedFilterApplyURLCombinesPredicates — реалистичный сохранённый
// фильтр несёт несколько условий сразу (severity + исключающий service) —
// проверяет, что ветки switch не затирают друг друга при совместном
// применении, а не только по одной в изоляции.
func TestLogSavedFilterApplyURLCombinesPredicates(t *testing.T) {
	f := logfilter.Filter{Predicates: []log.Predicate{
		{Field: log.FieldSeverity, Op: log.OpEq, Value: "warn"},
		{Field: log.FieldService, Op: log.OpNeq, Value: "worker"},
	}}
	q := parseLogsLink(t, logSavedFilterApplyURL(3, f), 3)
	if got := q["severity"]; len(got) != 1 || got[0] != "warn" {
		t.Fatalf("severity = %v, want [warn]", got)
	}
	if got := q["service_not"]; len(got) != 1 || got[0] != "worker" {
		t.Fatalf("service_not = %v, want [worker]", got)
	}
}
