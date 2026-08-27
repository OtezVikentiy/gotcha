package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/export"
)

// TestExportFilterSummaryNoFilters — пустые Params и ScopeIssueID=0 (Since и
// Until нулевые — RangeAll, см. докблок exportFilterSummary) дают «за всё
// время», а не пустую строку и не «без фильтров»: период — не «фильтр»,
// который можно пропустить молча, пользователь обязан видеть его всегда.
func TestExportFilterSummaryNoFilters(t *testing.T) {
	ctx := ruTestCtx()
	got := exportFilterSummary(ctx, export.Job{})
	if want := "за всё время"; got != want {
		t.Errorf("exportFilterSummary(пусто) = %q, want %q", got, want)
	}
}

// TestExportFilterSummaryIssueScope — заявка, ограниченная одной группой
// (ScopeIssueID != 0), показывает «issue #N», а период (Since/Until
// нулевые — RangeAll) добавляется следом тем же приёмом, что и везде.
func TestExportFilterSummaryIssueScope(t *testing.T) {
	ctx := ruTestCtx()
	got := exportFilterSummary(ctx, export.Job{ScopeIssueID: 42})
	if want := "issue #42, за всё время"; got != want {
		t.Errorf("exportFilterSummary(issue) = %q, want %q", got, want)
	}
}

// TestExportFilterSummaryStatusLevel — фильтры по статусу и уровню переводят
// значение через issues.status.*/issues.level.*, а не показывают сырой код.
func TestExportFilterSummaryStatusLevel(t *testing.T) {
	ctx := ruTestCtx()
	j := export.Job{Params: export.Params{Status: "resolved", Level: "error"}}
	got := exportFilterSummary(ctx, j)
	if want := "Решено, Ошибка, за всё время"; got != want {
		t.Errorf("exportFilterSummary(status+level) = %q, want %q", got, want)
	}
}

// TestExportFilterSummaryEnvironmentQuery — окружение и поисковый запрос
// подставляются в свои плейсхолдеры ({env}/{query}), а не теряются.
func TestExportFilterSummaryEnvironmentQuery(t *testing.T) {
	ctx := ruTestCtx()
	j := export.Job{Params: export.Params{Environment: "production", Query: "timeout"}}
	got := exportFilterSummary(ctx, j)
	if want := "env production, «timeout», за всё время"; got != want {
		t.Errorf("exportFilterSummary(env+query) = %q, want %q", got, want)
	}
}

// TestExportFilterSummaryPeriod — период форматируется через humanize.Time с
// time.UTC (числовой формат + подпись пояса), а не сырым t.Format: сводка
// строится в exportViewRow без параметра пояса зрителя, так что подмена на
// голый Format молча вернула бы время без метки часового пояса.
func TestExportFilterSummaryPeriod(t *testing.T) {
	ctx := ruTestCtx()
	since := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	until := time.Date(2026, 8, 21, 15, 30, 0, 0, time.UTC)
	j := export.Job{Params: export.Params{Since: since, Until: until}}
	got := exportFilterSummary(ctx, j)
	if want := "2026-08-20 10:00 UTC – 2026-08-21 15:30 UTC"; got != want {
		t.Errorf("exportFilterSummary(период) = %q, want %q", got, want)
	}
}

// TestExportFilterSummaryAllPartsJoined — все ветки разом идут через запятую
// в порядке issue → status → level → environment → query → период, как в
// exportFilterSummary; отдельный тест на то, что strings.Join не теряет и не
// переставляет части.
func TestExportFilterSummaryAllPartsJoined(t *testing.T) {
	ctx := ruTestCtx()
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	j := export.Job{
		ScopeIssueID: 7,
		Params: export.Params{
			Status:      "unresolved",
			Level:       "fatal",
			Environment: "staging",
			Query:       "boom",
			Since:       since,
			Until:       until,
		},
	}
	got := exportFilterSummary(ctx, j)
	want := "issue #7, Не решено, Критическая, env staging, «boom», " +
		"2026-01-01 00:00 UTC – 2026-01-02 00:00 UTC"
	if got != want {
		t.Errorf("exportFilterSummary(все фильтры) = %q, want %q", got, want)
	}
}

// TestExportFilterSummaryPeriodRequiresBothBounds — если развёрнут только
// один конец периода (второй остался нулевым — заявка ещё не прошла
// exportsCreate или это баг постановки), период в сводке не появляется:
// показывать половину диапазона хуже, чем не показывать его вовсе, а
// показать её как «за всё время» было бы прямой ложью — период не пуст,
// просто одна из границ ещё не развёрнута.
func TestExportFilterSummaryPeriodRequiresBothBounds(t *testing.T) {
	ctx := ruTestCtx()
	onlySince := export.Job{Params: export.Params{Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	if got, want := exportFilterSummary(ctx, onlySince), "без фильтров"; got != want {
		t.Errorf("exportFilterSummary(только Since) = %q, want %q", got, want)
	}
	onlyUntil := export.Job{Params: export.Params{Until: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}}
	if got, want := exportFilterSummary(ctx, onlyUntil), "без фильтров"; got != want {
		t.Errorf("exportFilterSummary(только Until) = %q, want %q", got, want)
	}
}
