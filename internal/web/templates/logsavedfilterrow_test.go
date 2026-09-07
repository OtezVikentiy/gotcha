package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
)

// renderLogSavedFilterRow рендерит одну строку панели сохранённых фильтров с
// правом редактирования (иначе форм «Обновить»/«Удалить» вовсе нет).
func renderLogSavedFilterRow(t *testing.T, filter LogsFilter, row LogSavedFilterRow) string {
	t.Helper()
	var buf bytes.Buffer
	if err := logSavedFilterRow(1, filter, row).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// deleteFormHTML вырезает разметку формы «Удалить» из отрендеренной строки
// панели — между её action-URL (кончается на "/delete") и ближайшим
// закрывающим </form>. Форма «Обновить» несёт то же скрытое поле, поэтому
// проверка по всему выводу не отличила бы починенную форму «Удалить» от
// всё ещё сломанной.
func deleteFormHTML(t *testing.T, html string) string {
	t.Helper()
	marker := `/delete"`
	idx := strings.Index(html, marker)
	if idx < 0 {
		t.Fatalf("форма «Удалить» не найдена в разметке: %s", html)
	}
	formStart := strings.LastIndex(html[:idx], "<form")
	if formStart < 0 {
		t.Fatalf("не нашли открывающий <form для «Удалить»: %s", html)
	}
	formEnd := strings.Index(html[idx:], "</form>")
	if formEnd < 0 {
		t.Fatalf("не нашли закрывающий </form для «Удалить»: %s", html)
	}
	return html[formStart : idx+formEnd+len("</form>")]
}

// TestLogSavedFilterRowDeleteFormCarriesViewConditions — находка финального
// ревью C5: форма «Удалить» обязана нести условия ТЕКУЩЕГО вида (то же
// скрытое поле, что и у формы «Обновить»), иначе web.logFilterFormParams(r)
// на сабмите видит пустую форму, и редирект после удаления уходит на голый
// /logs — набранные вручную исключения теряются, а на их месте молча
// подставляется фильтр по умолчанию.
func TestLogSavedFilterRowDeleteFormCarriesViewConditions(t *testing.T) {
	filter := LogsFilter{
		Not: []log.Predicate{{Field: log.FieldService, Op: log.OpNeq, Value: "worker"}},
	}
	row := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 7, Name: "рабочий", Applicable: true},
		CanEdit: true,
	}
	html := renderLogSavedFilterRow(t, filter, row)
	del := deleteFormHTML(t, html)
	if !strings.Contains(del, `name="service_not"`) || !strings.Contains(del, `value="worker"`) {
		t.Errorf("форма «Удалить» не несёт условие текущего вида: %s", del)
	}
}

// TestLogSavedFilterRowDefaultFormOmitsViewConditions — контрастная
// проверка: у формы «Сделать умолчанием» скрытое поле условий НАМЕРЕННО
// отсутствует (назначение умолчания — не действие над текущим видом),
// чтобы правка C5 не расползлась туда, где её не просили.
func TestLogSavedFilterRowDefaultFormOmitsViewConditions(t *testing.T) {
	filter := LogsFilter{
		Not: []log.Predicate{{Field: log.FieldService, Op: log.OpNeq, Value: "worker"}},
	}
	row := LogSavedFilterRow{
		Filter:    logfilter.Filter{ID: 7, Name: "рабочий", Applicable: true},
		CanEdit:   true,
		IsDefault: false,
	}
	html := renderLogSavedFilterRow(t, filter, row)
	marker := `/default"`
	idx := strings.Index(html, marker)
	if idx < 0 {
		t.Fatalf("форма «Сделать умолчанием» не найдена: %s", html)
	}
	formStart := strings.LastIndex(html[:idx], "<form")
	formEnd := strings.Index(html[idx:], "</form>")
	if formStart < 0 || formEnd < 0 {
		t.Fatalf("не нашли форму «Сделать умолчанием»: %s", html)
	}
	def := html[formStart : idx+formEnd+len("</form>")]
	if strings.Contains(def, `name="service_not"`) {
		t.Errorf("форма «Сделать умолчанием» неожиданно несёт условие текущего вида: %s", def)
	}
}
