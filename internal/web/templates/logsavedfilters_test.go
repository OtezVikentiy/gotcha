package templates

import (
	"bytes"
	"context"
	"strings"
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

// renderLogSavedFiltersSection рендерит панель целиком (заголовок, две группы,
// форма сохранения) — в отличие от renderLogSavedFilterRow, который берёт одну
// строку списка.
func renderLogSavedFiltersSection(t *testing.T, panel LogSavedFiltersPanel) string {
	t.Helper()
	var buf bytes.Buffer
	if err := logSavedFiltersSection(1, LogsFilter{}, panel, "").Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestLogSavedFiltersSectionCountsFilters — счётчик в заголовке свёрнутой
// панели считает ОБЕ группы: закрытая панель иначе выглядит одинаково и с
// фильтрами, и без них, и раскрывать её приходится наугад.
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

// TestLogSavedFiltersSectionOmitsCountWhenEmpty — на пустой панели счётчика
// нет вовсе: «0» в заголовке — это шум, а не сведение.
func TestLogSavedFiltersSectionOmitsCountWhenEmpty(t *testing.T) {
	html := renderLogSavedFiltersSection(t, LogSavedFiltersPanel{})
	if strings.Contains(html, "logs-saved-filters-count") {
		t.Errorf("пустая панель не должна нести счётчик:\n%s", html)
	}
}

// TestLogSavedFilterRowPutsEditFormInModal — поля правки (имя, видимость)
// живут в модалке, а не в строке списка: строка показывает только имя и
// действия. Проверяем и якорь-триггер, и то, что форма «Обновить» лежит
// ВНУТРИ разметки модалки — до правки оформления она стояла прямо в <li>,
// растягивая каждую строку списка полноширинным полем ввода.
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
