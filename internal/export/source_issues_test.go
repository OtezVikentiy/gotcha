package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestIssueSourceRecordHasAbsoluteURL проверяет, что Stream отдаёт Record
// с ровно набором колонок §6 спеки и что url — абсолютная ссылка на группу,
// а не относительный путь: файл открывают в почте и в таблице, где
// относительная ссылка ведёт в никуда.
func TestIssueSourceRecordHasAbsoluteURL(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-url", "boom", "app.worker", issue.LevelError, "prod", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	src := NewIssueSource(svc, "https://gotcha.example.com/")
	var records []Record
	if err := src.Stream(ctx, projectID, Params{}, func(r Record) error {
		records = append(records, r)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Stream вернул %d записей, want 1", len(records))
	}
	rec := records[0]

	wantURL := fmt.Sprintf("https://gotcha.example.com/projects/%d/issues/%d", projectID, res.IssueID)
	url, _ := rec["url"].(string)
	if url != wantURL {
		t.Errorf("url = %q, want %q (baseURL со слэшем не должен задваиваться)", url, wantURL)
	}

	if !strings.HasPrefix(url, "https://") {
		t.Errorf("url = %q не абсолютный", url)
	}
	idStr := strconv.FormatInt(res.IssueID, 10)
	if !strings.Contains(url, idStr) {
		t.Errorf("url = %q не содержит id группы %s", url, idStr)
	}

	// Ровно колонки §6, без лишних и без пропущенных.
	wantCols := IssueColumns()
	if len(rec) != len(wantCols) {
		t.Fatalf("Record содержит %d полей, IssueColumns() — %d: %v vs %v", len(rec), len(wantCols), rec, wantCols)
	}
	for _, c := range wantCols {
		if _, ok := rec[c]; !ok {
			t.Errorf("Record не содержит колонку %q", c)
		}
	}
	if rec["environments"] != "prod" {
		t.Errorf("environments = %v, want %q", rec["environments"], "prod")
	}
}

// TestIssueSourceEmptyEnvironmentsIsEmptyString — группа без окружения не
// должна получать плейсхолдер вроде "—" (это дело UI, не выгрузки): пустая
// строка в файле честнее и не путается со значением реального окружения.
func TestIssueSourceEmptyEnvironmentsIsEmptyString(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	if _, err := svc.Upsert(ctx, projectID, "fp-noenv", "boom", "", issue.LevelError, "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	src := NewIssueSource(svc, "https://gotcha.example.com")
	var got Record
	if err := src.Stream(ctx, projectID, Params{}, func(r Record) error {
		got = r
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got["environments"] != "" {
		t.Errorf("environments = %v, want пустую строку", got["environments"])
	}
}

// TestIssueSourceIsolatedByProject — чужой project_id не должен утекать в
// выдачу ни при каких параметрах фильтра: изоляция по проекту проверяется
// не только в issue.StreamForExport, но и на уровне источника выгрузки,
// который передаёт projectID дальше без потерь.
func TestIssueSourceIsolatedByProject(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectA, _ := seedProjectAndUser(t, pool)
	projectB, _ := seedProjectAndUser(t, pool)

	if _, err := svc.Upsert(ctx, projectA, "fp-a", "own", "", issue.LevelError, "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if _, err := svc.Upsert(ctx, projectB, "fp-b", "foreign", "", issue.LevelError, "", time.Now().UTC()); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	src := NewIssueSource(svc, "https://gotcha.example.com")
	var titles []string
	// Пустой Params — самый широкий фильтр, наибольший риск утечки.
	if err := src.Stream(ctx, projectA, Params{}, func(r Record) error {
		titles = append(titles, r["title"].(string))
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(titles) != 1 || titles[0] != "own" {
		t.Fatalf("Stream(projectA) = %v, чужая группа проекта B утекла", titles)
	}
}

// TestIssueSourcePipelineThroughWriter — сценарный тест полного пайплайна
// источник → Record → Writer: изолированные юниты источника и писателя могли
// бы поодиночке быть исправны и разойтись на стыке (например, в имени или
// типе колонки), а полный прогон через NDJSON это ловит.
func TestIssueSourcePipelineThroughWriter(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	seenAt := time.Date(2026, 8, 21, 9, 30, 0, 0, time.UTC)
	res, err := svc.Upsert(ctx, projectID, "fp-pipe", "boom in worker", "app.worker", issue.LevelWarning, "staging", seenAt)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := svc.Assign(ctx, res.IssueID, nil); err != nil {
		t.Fatalf("assign: %v", err)
	}

	src := NewIssueSource(svc, "https://gotcha.example.com")
	var buf bytes.Buffer
	w, err := NewWriter(&buf, FormatNDJSON, IssueColumns())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := src.Stream(ctx, projectID, Params{}, func(r Record) error {
		return w.Write(r)
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("NDJSON: %d строк, want 1: %q", len(lines), buf.String())
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("json.Unmarshal: %v (%q)", err, lines[0])
	}
	if row["title"] != "boom in worker" {
		t.Errorf("title = %v, want %q", row["title"], "boom in worker")
	}
	if row["culprit"] != "app.worker" {
		t.Errorf("culprit = %v, want %q", row["culprit"], "app.worker")
	}
	if row["level"] != string(issue.LevelWarning) {
		t.Errorf("level = %v, want %q", row["level"], issue.LevelWarning)
	}
	if row["status"] != "unresolved" {
		t.Errorf("status = %v, want %q", row["status"], "unresolved")
	}
	if row["environments"] != "staging" {
		t.Errorf("environments = %v, want %q", row["environments"], "staging")
	}
	if row["assignee_email"] != "" {
		t.Errorf("assignee_email = %v, want пустую строку (назначение снято)", row["assignee_email"])
	}
	wantURL := fmt.Sprintf("https://gotcha.example.com/projects/%d/issues/%d", projectID, res.IssueID)
	if row["url"] != wantURL {
		t.Errorf("url = %v, want %q", row["url"], wantURL)
	}
}
