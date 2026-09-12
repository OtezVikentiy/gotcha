package web

import (
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/export"
)

func TestExportFilterSummaryNoFilters(t *testing.T) {
	ctx := ruTestCtx()
	got := exportFilterSummary(ctx, export.Job{})
	if want := "за всё время"; got != want {
		t.Errorf("exportFilterSummary(пусто) = %q, want %q", got, want)
	}
}

func TestExportFilterSummaryIssueScope(t *testing.T) {
	ctx := ruTestCtx()
	got := exportFilterSummary(ctx, export.Job{ScopeIssueID: 42})
	if want := "issue #42, за всё время"; got != want {
		t.Errorf("exportFilterSummary(issue) = %q, want %q", got, want)
	}
}

func TestExportFilterSummaryStatusLevel(t *testing.T) {
	ctx := ruTestCtx()
	j := export.Job{Params: export.Params{Status: "resolved", Level: "error"}}
	got := exportFilterSummary(ctx, j)
	if want := "Решено, Ошибка, за всё время"; got != want {
		t.Errorf("exportFilterSummary(status+level) = %q, want %q", got, want)
	}
}

func TestExportFilterSummaryEnvironmentQuery(t *testing.T) {
	ctx := ruTestCtx()
	j := export.Job{Params: export.Params{Environment: "production", Query: "timeout"}}
	got := exportFilterSummary(ctx, j)
	if want := "env production, «timeout», за всё время"; got != want {
		t.Errorf("exportFilterSummary(env+query) = %q, want %q", got, want)
	}
}

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
