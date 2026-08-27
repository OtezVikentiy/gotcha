package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"

	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
)

// TestExportsListShowsStatusTruncationAndActions — по одной строке на статус,
// проверяем перевод статуса (queued/running/done/failed/expired), отметку
// «обрезана» и видимость кнопок download/delete по CanDownload/CanDelete —
// то же требование, что и брифа задачи 11 (страница «Выгрузки»).
func TestExportsListShowsStatusTruncationAndActions(t *testing.T) {
	rows := []ExportView{
		{ID: 1, KindLabel: "issues", FormatLabel: "csv", Status: "queued", CanDownload: false, CanDelete: false},
		{ID: 2, KindLabel: "issues", FormatLabel: "csv", Status: "running", CanDownload: false, CanDelete: false},
		{ID: 3, KindLabel: "events", FormatLabel: "ndjson", Status: "done", Truncated: true, CanDownload: true, CanDelete: true},
		{ID: 4, KindLabel: "events", FormatLabel: "json", Status: "failed", CanDownload: false, CanDelete: true},
		{ID: 5, KindLabel: "issues", FormatLabel: "csv", Status: "expired", CanDownload: false, CanDelete: true},
	}
	out := renderTo(t, Exports(7, rows, false, "u@e.com", true, "", nil))

	for _, want := range []string{"в очереди", "выполняется", "готово", "ошибка", "истекла", "обрезана"} {
		if !strings.Contains(out, want) {
			t.Errorf("на странице нет %q", want)
		}
	}
	// Ровно одна ссылка на скачивание — только у done-заявки с CanDownload.
	if n := strings.Count(out, `href="/projects/7/exports/3/download"`); n != 1 {
		t.Errorf("ссылок на скачивание заявки 3 = %d, want 1", n)
	}
	if strings.Contains(out, "/exports/1/download") || strings.Contains(out, "/exports/2/download") {
		t.Error("скачивание показано для незавершённой заявки")
	}
	// Удаление — форма только там, где CanDelete: 3, 4, 5.
	for _, id := range []string{"3", "4", "5"} {
		if !strings.Contains(out, `action="/projects/7/exports/`+id+`/delete"`) {
			t.Errorf("нет формы удаления заявки %s", id)
		}
	}
	// Queued/running (1, 2) — кнопки удаления быть не должно: заявка ещё
	// пишется воркером, см. докблок ExportView.CanDelete.
	if strings.Contains(out, "/exports/1/delete") || strings.Contains(out, "/exports/2/delete") {
		t.Error("кнопка удаления показана для выполняющейся заявки")
	}
}

// TestExportsListExpiresColumnShowsAbsoluteFutureTime — находка аудита
// P1-UX-1: ExpiresAt = finished_at + TTL, то есть ВСЕГДА в будущем;
// relativeTime (humanize.Ago) зажимает отрицательную разницу времени в ноль
// и печатает ключ time.just_now — весь семисуточный срок жизни файла
// колонка «Истекает» читалась бы как «файл истекает прямо сейчас», ровно
// противоположное правде. Мутация — вернуть @relativeTime(e.ExpiresAt) на
// место humanize.Time — обязана уронить оба ассерта: «только что» появится,
// абсолютная метка пропадёт.
func TestExportsListExpiresColumnShowsAbsoluteFutureTime(t *testing.T) {
	future := time.Date(2026, 9, 2, 16, 29, 0, 0, time.UTC)
	rows := []ExportView{
		{ID: 1, KindLabel: "issues", FormatLabel: "csv", Status: "done", ExpiresAt: future, CanDownload: true, CanDelete: true},
	}
	out := renderTo(t, Exports(7, rows, false, "u@e.com", true, "", nil))

	if strings.Contains(out, i18nT(t, "time.just_now")) {
		t.Errorf("колонка «Истекает» показывает %q для даты в будущем: %s", i18nT(t, "time.just_now"), out)
	}
	if !strings.Contains(out, "2026-09-02 16:29 UTC") {
		t.Errorf("нет абсолютной даты истечения (humanize.Time) на странице: %s", out)
	}
}

// TestExportsListPendingJobsShowDashForRowsAndSize — находка аудита
// P3-UX-8: у queued/running заявки Rows/Size — нули не потому, что
// выгрузка пуста, а потому, что воркер ещё не досчитал; «0 строк, 0Б»
// читается как «выгрузка пустая» (приём «—» в этой же таблице уже
// применяется к ExpiresAt.IsZero()). Различаем пендинг-строки от done
// подсчётом «—»: у пендинг-строки их ДВЕ (Размер + Истекает, у которой
// ExpiresAt тоже нулевое), у done — ОДНА (только Истекает, Размер уже
// «0B» настоящим значением) — так мутация, отключающая тире только в
// колонке «Размер», не потеряется на фоне независимого тире в «Истекает».
func TestExportsListPendingJobsShowDashForRowsAndSize(t *testing.T) {
	rows := []ExportView{
		// Author непустой везде намеренно: assigneeDisplay сама печатает «—»
		// для пустого email (issues.templ) — без Author этот независимый
		// источник «—» смешался бы со счётом ниже и испортил бы точную
		// проверку именно колонок «Размер»/«Строк».
		{ID: 1, KindLabel: "row-queued", FormatLabel: "csv", Status: "queued", Author: "u@e.com"},
		{ID: 2, KindLabel: "row-running", FormatLabel: "csv", Status: "running", Author: "u@e.com"},
		{ID: 3, KindLabel: "row-done", FormatLabel: "csv", Status: "done", Author: "u@e.com", CanDownload: true, CanDelete: true},
	}
	out := renderTo(t, Exports(7, rows, false, "u@e.com", true, "", nil))

	rowText := func(marker string) string {
		t.Helper()
		i := strings.Index(out, marker)
		if i < 0 {
			t.Fatalf("маркер %q не найден: %s", marker, out)
		}
		start := strings.LastIndex(out[:i], "<tr>")
		end := strings.Index(out[i:], "</tr>")
		if start < 0 || end < 0 {
			t.Fatalf("не удалось выделить <tr> вокруг маркера %q", marker)
		}
		return out[start : i+end]
	}

	for _, marker := range []string{"row-queued", "row-running"} {
		row := rowText(marker)
		if !strings.Contains(row, `<td class="num">—</td>`) {
			t.Errorf("строка %q: нет «—» в колонке «Строк»: %s", marker, row)
		}
		if strings.Contains(row, `<td class="num">0</td>`) {
			t.Errorf("строка %q: «0» вместо «—» в колонке «Строк» — читается как пустая выгрузка: %s", marker, row)
		}
		if n := strings.Count(row, "<td>—</td>"); n != 2 {
			t.Errorf("строка %q: «—» встречается %d раз, want 2 (Размер + Истекает): %s", marker, n, row)
		}
	}

	doneRow := rowText("row-done")
	if !strings.Contains(doneRow, `<td class="num">0</td>`) {
		t.Errorf("строка done: ожидали «0» в колонке «Строк» (настоящий нулевой результат): %s", doneRow)
	}
	if !strings.Contains(doneRow, "<td>0B</td>") {
		t.Errorf("строка done: ожидали «0B» в колонке «Размер»: %s", doneRow)
	}
	if n := strings.Count(doneRow, "<td>—</td>"); n != 1 {
		t.Errorf("строка done: «—» встречается %d раз, want 1 (только «Истекает», Размер уже настоящее значение): %s", n, doneRow)
	}
}

// TestExportsFormShowsErrorAndPreservesSubmittedValues — находка аудита
// P2-UX-4: раньше отказ постановки (kind/format невалиден, лимит частоты,
// лимит активных заявок, чужой scope_issue_id) уходил на chromeless
// ErrorPage с одной ссылкой «На главную» — отфильтрованный список заявок
// терялся, а errMsg рендерился как <h1> рядом с гигантской «422»/«429»
// (errorTitleKey не знает про эти статусы). h.renderExportsPage
// (exports.go) теперь перерисовывает ЭТУ страницу с errMsg над формой,
// открытой <details> (open?=) и восстановленными kind/format/include_pii —
// то же самое действие, которое пыталось произойти. Мутация — заменить
// open?={errMsg != ""} на open?={false} — обязана уронить ассерт про
// <details ... open>: без него errMsg провалился бы в свёрнутую <details>,
// невидимую без клика.
func TestExportsFormShowsErrorAndPreservesSubmittedValues(t *testing.T) {
	form := FormState{"kind": "events", "format": "ndjson", "include_pii": "1"}
	out := renderTo(t, Exports(7, nil, true, "u@e.com", true, "лимит частоты превышен", form))

	if !strings.Contains(out, "лимит частоты превышен") {
		t.Errorf("сообщение об ошибке не отрисовано: %s", out)
	}
	if !strings.Contains(out, `<details class="card" open>`) {
		t.Errorf("форма постановки не раскрыта автоматически при ошибке: %s", out)
	}
	if !strings.Contains(out, `<option value="events" selected>`) {
		t.Errorf("выбранный вид (events) не восстановлен в форме: %s", out)
	}
	if !strings.Contains(out, `<option value="ndjson" selected>`) {
		t.Errorf("выбранный формат (ndjson) не восстановлен в форме: %s", out)
	}
	if !strings.Contains(out, `name="include_pii" value="1" checked`) {
		t.Errorf("галка include_pii не восстановлена в форме: %s", out)
	}
}

// TestExportsFormNoErrorStaysCollapsedWithoutErrorText — обычный рендер
// страницы (errMsg="") не должен показывать пустой <p class="error"> и не
// должен раскрывать форму принудительно (open?={errMsg != ""} == false).
func TestExportsFormNoErrorStaysCollapsedWithoutErrorText(t *testing.T) {
	out := renderTo(t, Exports(7, nil, true, "u@e.com", true, "", nil))
	if strings.Contains(out, `<p class="error">`) {
		t.Errorf("пустое сообщение об ошибке всё равно отрисовано: %s", out)
	}
	if strings.Contains(out, `<details class="card" open>`) {
		t.Error("форма постановки раскрыта без ошибки — details обязана остаться свёрнутой")
	}
}

// TestExportsListEmptyState — пустой список даёт объяснение, а не голую
// таблицу без строк (тот же принцип, что и у issues/alert-suppression).
func TestExportsListEmptyState(t *testing.T) {
	out := renderTo(t, Exports(7, nil, false, "u@e.com", true, "", nil))
	if !strings.Contains(out, i18nT(t, "exports.empty.title")) {
		t.Error("нет пустого состояния")
	}
	if strings.Contains(out, "data-table") {
		t.Error("таблица отрисована при пустом списке")
	}
}

// TestExportsPageDisabledShowsExplanationNotEmptyList — enabled=false
// (h.Exports == nil на инстансе) обязана показывать объяснение, а не
// пустую таблицу заявок или форму постановки, которая всё равно вела бы на
// 404 (спека E1 §10; ревью веб-части, п.3). rows здесь непустой намеренно:
// проверяет, что ветка гейтится по enabled, а не молча по len(rows)==0.
func TestExportsPageDisabledShowsExplanationNotEmptyList(t *testing.T) {
	rows := []ExportView{{ID: 1, KindLabel: "issues", FormatLabel: "csv", Status: "done"}}
	out := renderTo(t, Exports(7, rows, true, "u@e.com", false, "", nil))

	if !strings.Contains(out, i18nT(t, "exports.disabled.title")) {
		t.Error("нет объяснения о выключенной фиче")
	}
	if strings.Contains(out, "data-table") {
		t.Error("таблица заявок отрисована при выключенной фиче")
	}
	if strings.Contains(out, `<form method="post" action="/projects/7/exports"`) {
		t.Error("форма постановки заявки отрисована при выключенной фиче")
	}
}

// TestExportsFormPIIHintGatedByCanManage — подсказка про необратимость
// маскирования показывается только тому, кому вообще доступна галка
// include_pii (CanManage) — оператору она молча игнорируется на бэкенде
// (exports.go:exportsCreate), значит и предлагать её незачем.
func TestExportsFormPIIHintGatedByCanManage(t *testing.T) {
	admin := renderTo(t, Exports(7, nil, true, "u@e.com", true, "", nil))
	if !strings.Contains(admin, i18nT(t, "exports.pii.hint")) {
		t.Error("админу не показана подсказка про необратимость маскирования")
	}
	if !strings.Contains(admin, `name="include_pii"`) {
		t.Error("админу не показана галка include_pii")
	}

	operator := renderTo(t, Exports(7, nil, false, "u@e.com", true, "", nil))
	if strings.Contains(operator, `name="include_pii"`) {
		t.Error("оператору показана галка include_pii — на бэкенде она для него не действует")
	}
}

// TestIssuesListExportButtonsGatedByCanOperate — кнопки экспорта на списке
// ошибок ведут на постановку заявки, которая требует requireProjectOperator;
// показывать их тому, для кого POST однозначно вернёт 404, — плохой UX (тот
// же принцип, что ExportView.CanDownload/CanDelete).
func TestIssuesListExportButtonsGatedByCanOperate(t *testing.T) {
	filter := IssuesFilter{Status: "unresolved", Level: "error", Query: "npe", Environment: "production", Sort: "last_seen"}

	withAccess := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true))
	if !strings.Contains(withAccess, `action="/projects/7/exports"`) {
		t.Error("оператору не показаны формы экспорта")
	}
	if !strings.Contains(withAccess, `value="npe"`) {
		t.Error("текущий поисковый запрос не пробрасывается скрытым полем формы экспорта")
	}

	without := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, false, false))
	if strings.Contains(without, `action="/projects/7/exports"`) {
		t.Error("без CanOperate формы экспорта не должны рендериться")
	}
}

// TestIssuesListExportFormsCarryTimeRange — период текущего списка обязан
// доехать до action формы экспорта query-параметром: hidden-поле POST-тела
// resolveTimeRange не видит (она читает только r.URL.Query()), без query
// заявка молча ставится по cookie/дефолту хендлера, а не по тому окну, что
// видит пользователь (ревью веб-части E1, п.1).
func TestIssuesListExportFormsCarryTimeRange(t *testing.T) {
	preset := IssuesFilter{Range: TimeRangeVM{Key: "7d"}}
	out := renderTo(t, IssuesList(7, nil, preset, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true))
	if n := strings.Count(out, `action="/projects/7/exports?period=7d"`); n != 2 {
		t.Errorf("форм с action=...?period=7d = %d, want 2 (группы + события)", n)
	}

	all := IssuesFilter{Range: TimeRangeVM{Key: "all", AllowAll: true}}
	outAll := renderTo(t, IssuesList(7, nil, all, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true))
	if n := strings.Count(outAll, `action="/projects/7/exports?period=all"`); n != 2 {
		t.Errorf("форм с action=...?period=all = %d, want 2", n)
	}

	custom := IssuesFilter{Range: TimeRangeVM{Key: "custom", Custom: true, Start: "2026-08-01T00:00", End: "2026-08-02T00:00"}}
	outCustom := renderTo(t, IssuesList(7, nil, custom, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true))
	// url.Values.Encode() сортирует ключи по алфавиту: end < period < start.
	if n := strings.Count(outCustom, `action="/projects/7/exports?end=2026-08-02T00%3A00&amp;period=custom&amp;start=2026-08-01T00%3A00"`); n != 2 {
		t.Errorf("форм с action=...?period=custom&start=...&end=... = %d, want 2: %s", n, outCustom)
	}
}

// TestIssueDetailHasExportForm — на детали issue есть форма экспорта её
// событий со скрытым scope_issue_id: без него постановка выгрузит события
// всего проекта, а не только этой issue.
func TestIssueDetailHasExportForm(t *testing.T) {
	it := issue.Issue{ID: 5, ProjectID: 7, Title: "NPE", Level: "error", Status: "unresolved"}
	stubC := templ.Raw("<svg data-c></svg>")
	out := renderTo(t, IssueDetail(it, nil, stubC, TimeRangeVM{Key: "24h"}, nil, "", nil, nil, "u@e.com", false, false, "", "", true, true))

	if !strings.Contains(out, `action="/projects/7/exports?period=24h"`) {
		t.Error("нет формы экспорта событий issue или период страницы не проброшен в action")
	}
	if !strings.Contains(out, `name="scope_issue_id" value="5"`) {
		t.Error("scope_issue_id не проброшен — выгрузка не будет ограничена этой issue")
	}
}

// TestIssueDetailHidesExportFormWhenExportsDisabled — на инстансе без
// каталога выгрузок (h.Exports == nil в web-слое, exportsEnabled=false)
// форма экспорта событий issue не должна рендериться вовсе: она вела бы на
// 404 (ревью веб-части E1, п.3).
func TestIssueDetailHidesExportFormWhenExportsDisabled(t *testing.T) {
	it := issue.Issue{ID: 5, ProjectID: 7, Title: "NPE", Level: "error", Status: "unresolved"}
	stubC := templ.Raw("<svg data-c></svg>")
	out := renderTo(t, IssueDetail(it, nil, stubC, TimeRangeVM{Key: "24h"}, nil, "", nil, nil, "u@e.com", false, false, "", "", false, false))

	if strings.Contains(out, "/exports?period=24h") {
		t.Error("форма экспорта показана при выключенной фиче (exportsEnabled=false)")
	}
}

// TestIssuesListExportFormsOfferFormatChoice — находка аудита P2-UX-3:
// раньше формат был зашит в hidden-поле кнопки и невидим («Экспорт групп» →
// csv, «Экспорт событий» → ndjson, пользователь не мог это изменить).
// Теперь обе точки входа несут <select name="format"> со всеми тремя
// значениями, а не фиксированный hidden-инпут — CSV событий по фильтру и
// JSON/NDJSON групп стали достижимы отсюда, не только с генерической формы
// страницы «Выгрузки» (у которой нет ни scope_issue_id, ни фильтров).
func TestIssuesListExportFormsOfferFormatChoice(t *testing.T) {
	filter := IssuesFilter{Status: "unresolved"}
	out := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true))

	if n := strings.Count(out, `<select name="format" class="select"`); n != 2 {
		t.Errorf(`селекторов format = %d, want 2 (группы + события): %s`, n, out)
	}
	// csv несёт selected (fallback выбора без сохранённого FormState —
	// P2-UX-4, form.Selected с nil-приёмником отдаёт "csv"), json/ndjson —
	// без атрибута: точный матч по всему тегу здесь и ловил бы регресс
	// набора опций, и не должен ломаться от появления selected на дефолте.
	for _, opt := range []string{`<option value="csv" selected>CSV</option>`, `<option value="json">JSON</option>`, `<option value="ndjson">NDJSON</option>`} {
		if n := strings.Count(out, opt); n != 2 {
			t.Errorf("опция %q встречается %d раз, want 2 (по одной форме на кнопку)", opt, n)
		}
	}
	// Мутация «формат зашит в hidden» обязана уронить оба ассерта выше:
	// hidden-поле с форматом на этих кнопках не должно остаться вовсе.
	if strings.Contains(out, `<input type="hidden" name="format"`) {
		t.Error("формат всё ещё зашит в hidden-поле — выбора нет")
	}
}

// TestIssuesListExportFormsGatePIIByCanManage — галка include_pii на
// раскрытых кнопках экспорта видна только CanManage (та же роль, что и
// authz.CanManage в exports.go, — от оператора параметр молча
// игнорируется на постановке). До находки P2-UX-3 галки на этих кнопках не
// было ВООБЩЕ, даже для CanManage — PII работал только на генерической
// форме страницы «Выгрузки» без scope_issue_id/фильтров.
func TestIssuesListExportFormsGatePIIByCanManage(t *testing.T) {
	filter := IssuesFilter{Status: "unresolved"}

	admin := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, true))
	if n := strings.Count(admin, `name="include_pii"`); n != 2 {
		t.Errorf("галок include_pii у CanManage = %d, want 2 (группы + события): %s", n, admin)
	}

	operator := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true, false))
	if strings.Contains(operator, `name="include_pii"`) {
		t.Error("оператору без CanManage показана галка include_pii — на бэкенде она для него не действует")
	}
	// canOperate всё ещё true у operator — кнопки экспорта сами должны
	// остаться на месте, гейтится только галка PII.
	if n := strings.Count(operator, `<select name="format" class="select"`); n != 2 {
		t.Errorf("у оператора без CanManage должны остаться обе кнопки экспорта, селекторов format = %d, want 2", n)
	}
}

// TestIssueDetailExportFormOffersFormatAndPII — та же находка P2-UX-3 на
// третьей точке входа (карточка issue): раньше формат был зашит в
// hidden-поле («Экспорт событий issue» всегда давал csv) и PII-галки не
// было — то есть выгрузить события ОДНОЙ группы без маски было нельзя
// вовсе, даже владельцу организации.
func TestIssueDetailExportFormOffersFormatAndPII(t *testing.T) {
	it := issue.Issue{ID: 5, ProjectID: 7, Title: "NPE", Level: "error", Status: "unresolved"}
	stubC := templ.Raw("<svg data-c></svg>")

	admin := renderTo(t, IssueDetail(it, nil, stubC, TimeRangeVM{Key: "24h"}, nil, "", nil, nil, "u@e.com", false, false, "", "", true, true))
	if !strings.Contains(admin, `<select name="format" class="select"`) {
		t.Error("нет выбора формата на форме экспорта событий issue")
	}
	if strings.Contains(admin, `<input type="hidden" name="format"`) {
		t.Error("формат на форме экспорта событий issue всё ещё зашит в hidden-поле")
	}
	if !strings.Contains(admin, `name="include_pii"`) {
		t.Error("CanManage не видит галку include_pii на форме экспорта событий issue")
	}
	// scope_issue_id обязан остаться рядом с новыми полями — иначе PII/формат
	// достались бы ценой контекста точки входа.
	if !strings.Contains(admin, `name="scope_issue_id" value="5"`) {
		t.Error("scope_issue_id потерян при добавлении формата/PII")
	}

	operator := renderTo(t, IssueDetail(it, nil, stubC, TimeRangeVM{Key: "24h"}, nil, "", nil, nil, "u@e.com", false, false, "", "", true, false))
	if strings.Contains(operator, `name="include_pii"`) {
		t.Error("оператору без CanManage показана галка include_pii на форме экспорта событий issue")
	}
}

// TestExportsIntroExplainsRefreshAndEmail — находка аудита P2-UX-6: спека
// §3 закрепляет «обновление статуса — перезагрузкой страницы», но до
// человека это не доезжало — ни флеш, ни exports.intro об этом не
// говорили, и о письме по готовности/отказу тоже не упоминалось. Мутация —
// вернуть exports.intro к прежнему тексту без этих двух фактов — обязана
// уронить оба ассерта.
func TestExportsIntroExplainsRefreshAndEmail(t *testing.T) {
	out := renderTo(t, Exports(7, nil, false, "u@e.com", true, "", nil))
	if !strings.Contains(out, "перезагрузите страницу") {
		t.Errorf("exports.intro не объясняет, что статус нужно обновлять перезагрузкой страницы: %s", out)
	}
	if !strings.Contains(out, "письма") {
		t.Errorf("exports.intro не упоминает письмо о готовности/отказе: %s", out)
	}
}

// TestExportsTerminologyMatchesIssuesNav — находка аудита P3-UX-10: раздел
// в навигации называется «Проблемы» (nav.issues/issues.title), а фича
// экспорта раньше говорила «ошибки» — «Группы ошибок» в селекторе вида и
// «со списка ошибок» в пустом состоянии, при том что кнопки на самом деле
// называются «Экспорт групп»/«Экспорт событий», не «Экспорт». Мутация —
// вернуть exports.kind.issues к "Группы ошибок" — обязана уронить первый
// ассерт.
func TestExportsTerminologyMatchesIssuesNav(t *testing.T) {
	if got := i18nT(t, "exports.kind.issues"); got != "Группы проблем" {
		t.Errorf(`exports.kind.issues = %q, want "Группы проблем" (термин раздела "Проблемы", не "ошибки")`, got)
	}
	empty := renderTo(t, Exports(7, nil, false, "u@e.com", true, "", nil))
	if strings.Contains(empty, "списка ошибок") {
		t.Errorf("пустое состояние всё ещё называет список ошибками, а не проблемами: %s", empty)
	}
	if strings.Contains(empty, "кнопкой «Экспорт»") {
		t.Errorf("пустое состояние ссылается на несуществующую кнопку «Экспорт»: %s", empty)
	}
	if !strings.Contains(empty, "«Экспорт групп»") || !strings.Contains(empty, "«Экспорт событий»") {
		t.Errorf("пустое состояние не называет реальные кнопки («Экспорт групп»/«Экспорт событий»): %s", empty)
	}
}

// TestExportsTableHeadersUseUnifiedKeyScheme — находка аудита P3-UX-12:
// заголовки колонок были разложены по двум схемам ключей — kind/format/
// filters/status/pii/created несли префикс "exports.table.", а rows/size/
// expires/author были плоскими "exports.*" — тот же класс сущностей
// (заголовок столбца одной и той же таблицы), разные имена. Ключи rows/
// size/expires/author переехали под "exports.table.*". i18n.T падает на
// сам переданный ключ, если перевода нет (см. её докблок) — тест ловит и
// опечатку в новом имени, и случайный откат на старую схему.
func TestExportsTableHeadersUseUnifiedKeyScheme(t *testing.T) {
	for _, key := range []string{
		"exports.table.kind", "exports.table.format", "exports.table.filters",
		"exports.table.status", "exports.table.rows", "exports.table.size",
		"exports.table.pii", "exports.table.created", "exports.table.expires",
		"exports.table.author",
	} {
		if got := i18nT(t, key); got == key {
			t.Errorf("ключ %q не переведён (T вернул сам ключ) — заголовки колонок не унифицированы под exports.table.*", key)
		}
	}
}

// i18nT — перевод ключа в той же локали, что renderTo (ru), для сравнения с
// отрисованным телом.
func i18nT(t *testing.T, key string) string {
	t.Helper()
	return i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), key)
}

// TestExportsListShowsFailureReasonHintForKnownKey — P2-UX-2 аудита:
// провалившаяся заявка обязана сообщить причину прямо на странице выгрузок,
// не только в письме. web-слой (exportViewRow) уже сверяет ключ по
// export.KnownFailureReasonKey и отдаёт сюда только известный — здесь
// проверяется только рендер: подсказка есть у заявки с непустым
// FailureReasonKey и её нет у заявки без ключа (queued/старая failed-строка
// без миграции). Мутация — убрать блок `if e.FailureReasonKey != ""` в
// exports.templ (оставить только Truncated) — обязана уронить первую
// проверку (текст причины пропадёт со страницы).
func TestExportsListShowsFailureReasonHintForKnownKey(t *testing.T) {
	rows := []ExportView{
		{ID: 1, KindLabel: "issues", FormatLabel: "csv", Status: "failed", FailureReasonKey: "exports.mail.failed.reason.disk_full", CanDelete: true},
		{ID: 2, KindLabel: "issues", FormatLabel: "csv", Status: "failed", CanDelete: true},
	}
	out := renderTo(t, Exports(7, rows, false, "u@e.com", true, "", nil))

	want := i18n.Tf(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}),
		"exports.failed.reason", "reason", i18nT(t, "exports.mail.failed.reason.disk_full"))
	if !strings.Contains(out, want) {
		t.Errorf("на странице нет подсказки причины %q: %s", want, out)
	}
	// Заявка 2 без ключа не должна показать ни подсказку, ни служебный текст
	// вида "Причина: " без содержимого.
	if strings.Count(out, "Причина:") != 1 {
		t.Errorf("подсказка причины показана не ровно один раз: %s", out)
	}
}

// exportRow сам НЕ проверяет FailureReasonKey на принадлежность известному
// множеству — переводит любое непустое значение через i18n.T(ctx, ...) как
// есть (тот же приём, что и Status/"exports.status."+Status). Защита от
// утечки чужого/битого ключа как техтекста живёт ОДНИМ уровнем выше, в
// exportViewRow (internal/web/exports.go, export.KnownFailureReasonKey) —
// её и мутационно проверяет internal/web/exports_test.go на реальном
// HTTP-рендере страницы, где имеет смысл: заявка, у которой в БД случайно
// оказался неизвестный ключ, а не искусственно собранный ExportView здесь.
