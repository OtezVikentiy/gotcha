package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
)

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

func renderLogSavedFiltersSection(t *testing.T, panel LogSavedFiltersPanel) string {
	t.Helper()
	var buf bytes.Buffer
	if err := logSavedFiltersSection(1, LogsFilter{}, panel, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestLogSavedFiltersSectionCountsFilters(t *testing.T) {
	panel := LogSavedFiltersPanel{
		Personal: []LogSavedFilterRow{
			{Filter: logfilter.Filter{ID: 1, Name: "мой A", Applicable: true}},
			{Filter: logfilter.Filter{ID: 2, Name: "мой B", Applicable: true}},
		},
		Shared: []LogSavedFilterRow{
			{Filter: logfilter.Filter{ID: 3, Name: "общий", Applicable: true}},
		},
	}
	html := renderLogSavedFiltersSection(t, panel)
	const want = `<span class="logs-saved-filters-count">3</span>`
	if !strings.Contains(html, want) {
		t.Errorf("счётчик заголовка: нет %s\n%s", want, html)
	}
}

func TestLogSavedFiltersSectionOmitsCountWhenEmpty(t *testing.T) {
	html := renderLogSavedFiltersSection(t, LogSavedFiltersPanel{})
	if strings.Contains(html, "logs-saved-filters-count") {
		t.Errorf("пустая панель не должна нести счётчик:\n%s", html)
	}
}

func TestLogSavedFilterRowPutsEditFormInModal(t *testing.T) {
	row := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 7, Name: "шумный nginx", Applicable: true},
		CanEdit: true,
	}
	html := renderLogSavedFilterRow(t, LogsFilter{}, row, true)

	trigger := `href="#` + logSavedFilterEditModalID(7) + `"`
	if !strings.Contains(html, trigger) {
		t.Errorf("нет ссылки-триггера модалки %s\n%s", trigger, html)
	}
	modalStart := strings.Index(html, `<div id="`+logSavedFilterEditModalID(7)+`"`)
	if modalStart < 0 {
		t.Fatalf("модалка правки не отрисована:\n%s", html)
	}
	formStart := strings.Index(html, `/filters/7/update"`)
	if formStart < 0 {
		t.Fatalf("форма «Обновить» не найдена:\n%s", html)
	}
	if formStart < modalStart {
		t.Errorf("форма «Обновить» стоит в строке списка, а не внутри модалки:\n%s", html)
	}
}
