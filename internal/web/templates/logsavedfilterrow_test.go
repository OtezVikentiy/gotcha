package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
	"gitflic.ru/otezvikentiy/gotcha/internal/logfilter"
)

func renderLogSavedFilterRow(t *testing.T, filter LogsFilter, row LogSavedFilterRow, canShare bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := logSavedFilterRow(1, filter, row, canShare).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func updateFormHTML(t *testing.T, html string) string {
	t.Helper()
	marker := `/update"`
	idx := strings.Index(html, marker)
	if idx < 0 {
		t.Fatalf("форма «Обновить» не найдена в разметке: %s", html)
	}
	formStart := strings.LastIndex(html[:idx], "<form")
	if formStart < 0 {
		t.Fatalf("не нашли открывающий <form для «Обновить»: %s", html)
	}
	formEnd := strings.Index(html[idx:], "</form>")
	if formEnd < 0 {
		t.Fatalf("не нашли закрывающий </form для «Обновить»: %s", html)
	}
	return html[formStart : idx+formEnd+len("</form>")]
}

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

func TestLogSavedFilterRowDeleteFormCarriesViewConditions(t *testing.T) {
	filter := LogsFilter{
		Not: []log.Predicate{{Field: log.FieldService, Op: log.OpNeq, Value: "worker"}},
	}
	row := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 7, Name: "рабочий", Applicable: true},
		CanEdit: true,
	}
	html := renderLogSavedFilterRow(t, filter, row, true)
	del := deleteFormHTML(t, html)
	if !strings.Contains(del, `name="service_not"`) || !strings.Contains(del, `value="worker"`) {
		t.Errorf("форма «Удалить» не несёт условие текущего вида: %s", del)
	}
}

func TestLogSavedFilterRowDefaultFormOmitsViewConditions(t *testing.T) {
	filter := LogsFilter{
		Not: []log.Predicate{{Field: log.FieldService, Op: log.OpNeq, Value: "worker"}},
	}
	row := LogSavedFilterRow{
		Filter:    logfilter.Filter{ID: 7, Name: "рабочий", Applicable: true},
		CanEdit:   true,
		IsDefault: false,
	}
	html := renderLogSavedFilterRow(t, filter, row, true)
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

func TestLogSavedFilterRowUpdateFormHasRenameField(t *testing.T) {
	row := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 7, Name: "рабочий", Applicable: true},
		CanEdit: true,
	}
	html := renderLogSavedFilterRow(t, LogsFilter{}, row, true)
	upd := updateFormHTML(t, html)
	if strings.Contains(upd, `type="hidden" name="name"`) {
		t.Errorf("поле имени всё ещё скрытое, переименовать нельзя: %s", upd)
	}
	if !strings.Contains(upd, `type="text" name="name"`) || !strings.Contains(upd, `value="рабочий"`) {
		t.Errorf("нет текстового поля имени, предзаполненного текущим значением: %s", upd)
	}
}

func TestLogSavedFilterRowUpdateFormHasVisibilityToggleWhenCanShare(t *testing.T) {
	ownerID := int64(42)
	personal := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 7, Name: "личный", Applicable: true, OwnerUserID: &ownerID},
		CanEdit: true,
	}
	html := renderLogSavedFilterRow(t, LogsFilter{}, personal, true)
	upd := updateFormHTML(t, html)
	if !strings.Contains(upd, `type="checkbox" name="shared"`) {
		t.Fatalf("нет переключателя видимости при canShare=true: %s", upd)
	}
	if strings.Contains(upd, `type="checkbox" name="shared" value="1" checked`) {
		t.Errorf("личный фильтр показан отмеченным (общим) переключателем: %s", upd)
	}

	shared := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 8, Name: "общий", Applicable: true, OwnerUserID: nil},
		CanEdit: true,
	}
	htmlShared := renderLogSavedFilterRow(t, LogsFilter{}, shared, true)
	updShared := updateFormHTML(t, htmlShared)
	if !strings.Contains(updShared, `type="checkbox" name="shared" value="1" checked`) {
		t.Errorf("общий фильтр показан не отмеченным переключателем: %s", updShared)
	}
}

func TestLogSavedFilterRowUpdateFormOmitsVisibilityToggleWithoutCanShare(t *testing.T) {
	ownerID := int64(42)
	row := LogSavedFilterRow{
		Filter:  logfilter.Filter{ID: 7, Name: "личный", Applicable: true, OwnerUserID: &ownerID},
		CanEdit: true,
	}
	html := renderLogSavedFilterRow(t, LogsFilter{}, row, false)
	upd := updateFormHTML(t, html)
	if strings.Contains(upd, `type="checkbox" name="shared"`) {
		t.Errorf("переключатель видимости показан рядовому участнику (canShare=false): %s", upd)
	}
}
