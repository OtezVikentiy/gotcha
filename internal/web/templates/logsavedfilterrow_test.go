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
// canShare — то же право, что у panel.CanShare (заводить/менять видимость
// общих фильтров): управляет тем, показан ли переключатель видимости в
// форме «Обновить» (находка финального ревью C10).
func renderLogSavedFilterRow(t *testing.T, filter LogsFilter, row LogSavedFilterRow, canShare bool) string {
	t.Helper()
	var buf bytes.Buffer
	if err := logSavedFilterRow(1, filter, row, canShare).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// updateFormHTML вырезает разметку формы «Обновить» — между её action-URL
// (кончается на "/update") и ближайшим закрывающим </form>.
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
	html := renderLogSavedFilterRow(t, filter, row, true)
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

// TestLogSavedFilterRowUpdateFormHasRenameField — находка финального ревью
// C10: §7.3 спеки объявляет переименование отдельным действием панели,
// хендлер (logFiltersUpdate) его уже поддерживает, но форма «Обновить»
// несла имя СКРЫТЫМ полем со старым значением — переименовать можно было,
// только создав второй фильтр. Поле обязано быть текстовым и предзаполнено
// текущим именем (не пустым — иначе сабмит без правки стёр бы имя).
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

// TestLogSavedFilterRowUpdateFormHasVisibilityToggleWhenCanShare — вторая
// половина C10: смена видимости (личный ↔ общий) хендлером поддержана
// (см. Store.Update и requireLogFilterOperator), но управлять ей было
// нечем — эхо-поле "shared" меняло значение только вместе с самим полем,
// никогда пользователем. Чекбокс обязан присутствовать, когда у смотрящего
// есть право на общие фильтры (тот же уровень, что у создания), и отражать
// ТЕКУЩЕЕ состояние через checked.
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

// TestLogSavedFilterRowUpdateFormOmitsVisibilityToggleWithoutCanShare —
// рядовому участнику (canShare=false) переключатель видимости не
// показывается вовсе: подмена значения формы всё равно отклонится
// requireLogFilterOperator веб-слоем, но элемент управления, ведущий к
// гарантированному 403, вводит в заблуждение и не должен рендериться.
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
