package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestIssueColumnsContractPin — заголовок CSV выгрузки issues это ПУБЛИЧНЫЙ
// контракт: после 1.0 переименование колонки ломает чужие парсеры (аудит
// 2026-08-27, DEDUP-P1 кластер 5). Список ниже — ЛИТЕРАЛ, набранный руками,
// а НЕ вызов IssueColumns() — раньше единственная проверка контракта
// (TestIssueSourceRecordHasAbsoluteURL ниже) сверяла Record с тем же самым
// IssueColumns(), из которого код и берёт колонки: переименование колонки
// в IssueColumns() проходило тест зелёным, потому что «ожидание» менялось
// вместе с «реализацией» одним и тем же изменением одной строки.
//
// Порядок ТОЖЕ часть контракта: CSV-писатель кладёт значения по порядку
// этого среза (см. docblock IssueColumns), поэтому сравнение — поэлементное,
// не через множество.
//
// Менять этот литерал можно только осознанно, вместе с записью в CHANGELOG —
// это предупреждение потребителям файла, а не деталь реализации.
func TestIssueColumnsContractPin(t *testing.T) {
	want := []string{"id", "title", "culprit", "level", "status", "times_seen",
		"first_seen", "last_seen", "environments", "assignee_email", "url"}
	got := IssueColumns()
	if len(got) != len(want) {
		t.Fatalf("IssueColumns() = %v (%d колонок), want %v (%d) — контракт CSV изменился без записи в CHANGELOG?", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("IssueColumns()[%d] = %q, want %q — переименование/перестановка публичной колонки требует записи в CHANGELOG", i, got[i], want[i])
		}
	}
}

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
	if err := src.Stream(ctx, projectID, true, Params{}, func(r Record) error {
		records = append(records, r)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Stream вернул %d записей, want 1", len(records))
	}
	rec := records[0]

	// Ожидание собирается ЛИТЕРАЛЬНО, а не тем же выражением, что и
	// реализация: пока здесь стоял fmt.Sprintf с тем же шаблоном, тест был
	// тавтологией и пропустил в прод ссылку на несуществующий
	// /projects/{id}/issues/{id} (v0.22.0). Что путь реально обслуживается
	// роутером, проверяет TestExportIssueURLHitsRegisteredRoute в
	// internal/web — здесь роутера нет.
	wantURL := "https://gotcha.example.com/issues/" + strconv.FormatInt(res.IssueID, 10)
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
	if err := src.Stream(ctx, projectID, true, Params{}, func(r Record) error {
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
	if err := src.Stream(ctx, projectA, true, Params{}, func(r Record) error {
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
	if err := src.Stream(ctx, projectID, true, Params{}, func(r Record) error {
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
	wantURL := "https://gotcha.example.com/issues/" + strconv.FormatInt(res.IssueID, 10)
	if row["url"] != wantURL {
		t.Errorf("url = %v, want %q", row["url"], wantURL)
	}
}

// TestIssueSourceOrderDescendingByIDOnEqualLastSeen — F2 контрактной уборки
// 2026-08-28 (CONTRACT-DECISIONS.md): порядок ORDER BY last_seen DESC,
// id DESC (issue.Service.StreamForExport, internal/issue/query.go) —
// публичный контракт выгрузки, до этого державшийся только докблоком.
// Несколько групп с ОДИНАКОВЫМ last_seen обязаны прийти через IssueSource
// строго убывающим по id — тай-брейк, без которого порядок строк выгрузки
// переставал бы быть детерминированным на границе одинаковых значений
// last_seen (частый случай: пачка событий, пришедшая разом).
func TestIssueSourceOrderDescendingByIDOnEqualLastSeen(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	same := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	const n = 12
	var wantIDs []int64
	for i := 0; i < n; i++ {
		res, err := svc.Upsert(ctx, projectID, fmt.Sprintf("fp-order-%02d", i), "boom", "", issue.LevelError, "", same)
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
		wantIDs = append(wantIDs, res.IssueID)
	}
	// Убывающий порядок по id — ожидание собирается сортировкой уже
	// известных id, НЕ повторением запроса реализации: тавтология здесь
	// пропустила бы потерю тай-брейка так же тихо, как раньше пропускала
	// потерю ссылки TestIssueSourceRecordHasAbsoluteURL (см. её докблок).
	sort.Slice(wantIDs, func(i, j int) bool { return wantIDs[i] > wantIDs[j] })

	src := NewIssueSource(svc, "https://gotcha.example.com")
	var gotIDs []int64
	if err := src.Stream(ctx, projectID, true, Params{}, func(r Record) error {
		id, _ := r["id"].(int64)
		gotIDs = append(gotIDs, id)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(gotIDs) != n {
		t.Fatalf("получено %d записей, want %d", len(gotIDs), n)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("порядок строк на равном last_seen не убывает строго по id (позиция %d): got %d, want %d — полные последовательности: got=%v, want=%v",
				i, gotIDs[i], wantIDs[i], gotIDs, wantIDs)
		}
	}
}

// TestIssueSourceMasksAssigneeEmailByDefault — K4-1 (аудит перед 1.0):
// assignee_email — прямой идентификатор пользователя (email назначенного),
// как user_email в выгрузке событий, и обязан маскироваться MaskUser при
// includePII == false, а не уезжать как есть независимо от галки заявки.
func TestIssueSourceMasksAssigneeEmailByDefault(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	assigneeEmail := "assignee-" + randSlug(t) + "@e.com"
	var assigneeID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id`,
		assigneeEmail).Scan(&assigneeID); err != nil {
		t.Fatalf("insert assignee: %v", err)
	}

	res, err := svc.Upsert(ctx, projectID, "fp-assignee", "boom", "app.worker",
		issue.LevelError, "prod", time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := svc.Assign(ctx, res.IssueID, &assigneeID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	src := NewIssueSource(svc, "https://gotcha.example.com")

	var masked []Record
	if err := src.Stream(ctx, projectID, false, Params{}, func(r Record) error {
		masked = append(masked, r)
		return nil
	}); err != nil {
		t.Fatalf("Stream(includePII=false): %v", err)
	}
	if len(masked) != 1 {
		t.Fatalf("includePII=false: получили %d записей, want 1", len(masked))
	}
	if got, _ := masked[0]["assignee_email"].(string); got != "[masked]" {
		t.Errorf("assignee_email (includePII=false) = %q, want [masked]", got)
	}

	var unmasked []Record
	if err := src.Stream(ctx, projectID, true, Params{}, func(r Record) error {
		unmasked = append(unmasked, r)
		return nil
	}); err != nil {
		t.Fatalf("Stream(includePII=true): %v", err)
	}
	if len(unmasked) != 1 {
		t.Fatalf("includePII=true: получили %d записей, want 1", len(unmasked))
	}
	if got, _ := unmasked[0]["assignee_email"].(string); got != assigneeEmail {
		t.Errorf("assignee_email (includePII=true) = %q, want %q", got, assigneeEmail)
	}
}

// TestIssueSourceEmptyAssigneeEmailNotMasked — пустая колонка (группа без
// назначенного) не должна подменяться маской ни в одном из режимов: иначе
// по выгрузке нельзя было бы отличить «не назначено» от «email скрыт»
// (симметрично докблоку MaskUser в pii.go).
func TestIssueSourceEmptyAssigneeEmailNotMasked(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	projectID, _ := seedProjectAndUser(t, pool)

	if _, err := svc.Upsert(ctx, projectID, "fp-unassigned", "boom", "app.worker",
		issue.LevelError, "prod", time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	src := NewIssueSource(svc, "https://gotcha.example.com")
	for _, includePII := range []bool{false, true} {
		var records []Record
		if err := src.Stream(ctx, projectID, includePII, Params{}, func(r Record) error {
			records = append(records, r)
			return nil
		}); err != nil {
			t.Fatalf("Stream(includePII=%v): %v", includePII, err)
		}
		if len(records) != 1 {
			t.Fatalf("includePII=%v: получили %d записей, want 1", includePII, len(records))
		}
		if got, _ := records[0]["assignee_email"].(string); got != "" {
			t.Errorf("includePII=%v: assignee_email = %q, want \"\" (нет назначенного)", includePII, got)
		}
	}
}
