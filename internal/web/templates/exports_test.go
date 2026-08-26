package templates

import (
	"context"
	"strings"
	"testing"

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
	out := renderTo(t, Exports(7, rows, false, "u@e.com"))

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

// TestExportsListEmptyState — пустой список даёт объяснение, а не голую
// таблицу без строк (тот же принцип, что и у issues/alert-suppression).
func TestExportsListEmptyState(t *testing.T) {
	out := renderTo(t, Exports(7, nil, false, "u@e.com"))
	if !strings.Contains(out, i18nT(t, "exports.empty.title")) {
		t.Error("нет пустого состояния")
	}
	if strings.Contains(out, "data-table") {
		t.Error("таблица отрисована при пустом списке")
	}
}

// TestExportsFormPIIHintGatedByCanManage — подсказка про необратимость
// маскирования показывается только тому, кому вообще доступна галка
// include_pii (CanManage) — оператору она молча игнорируется на бэкенде
// (exports.go:exportsCreate), значит и предлагать её незачем.
func TestExportsFormPIIHintGatedByCanManage(t *testing.T) {
	admin := renderTo(t, Exports(7, nil, true, "u@e.com"))
	if !strings.Contains(admin, i18nT(t, "exports.pii.hint")) {
		t.Error("админу не показана подсказка про необратимость маскирования")
	}
	if !strings.Contains(admin, `name="include_pii"`) {
		t.Error("админу не показана галка include_pii")
	}

	operator := renderTo(t, Exports(7, nil, false, "u@e.com"))
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

	withAccess := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, true))
	if !strings.Contains(withAccess, `action="/projects/7/exports"`) {
		t.Error("оператору не показаны формы экспорта")
	}
	if !strings.Contains(withAccess, `value="npe"`) {
		t.Error("текущий поисковый запрос не пробрасывается скрытым полем формы экспорта")
	}

	without := renderTo(t, IssuesList(7, nil, filter, 1, 0, "u@e.com", nil, nil, GettingStartedVM{}, false))
	if strings.Contains(without, `action="/projects/7/exports"`) {
		t.Error("без CanOperate формы экспорта не должны рендериться")
	}
}

// TestIssueDetailHasExportForm — на детали issue есть форма экспорта её
// событий со скрытым scope_issue_id: без него постановка выгрузит события
// всего проекта, а не только этой issue.
func TestIssueDetailHasExportForm(t *testing.T) {
	it := issue.Issue{ID: 5, ProjectID: 7, Title: "NPE", Level: "error", Status: "unresolved"}
	stubC := templ.Raw("<svg data-c></svg>")
	out := renderTo(t, IssueDetail(it, nil, stubC, TimeRangeVM{Key: "24h"}, nil, "", nil, nil, "u@e.com", false, false, "", ""))

	if !strings.Contains(out, `action="/projects/7/exports"`) {
		t.Error("нет формы экспорта событий issue")
	}
	if !strings.Contains(out, `name="scope_issue_id" value="5"`) {
		t.Error("scope_issue_id не проброшен — выгрузка не будет ограничена этой issue")
	}
}

// i18nT — перевод ключа в той же локали, что renderTo (ru), для сравнения с
// отрисованным телом.
func i18nT(t *testing.T, key string) string {
	t.Helper()
	return i18n.T(i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"}), key)
}
