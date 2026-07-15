package trace_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// newPerfProject: прямые вставки — пакет trace не зависит от org.
func newPerfProject(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	ctx := context.Background()
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id",
		slug+"@example.com").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,$1,1000000) RETURNING id",
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$2) RETURNING id",
		orgID, slug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func nPlusOneFinding() trace.Finding {
	return trace.Finding{
		Kind:        trace.KindNPlusOne,
		Title:       "N+1 запросов: SELECT * FROM users WHERE id = ?",
		Culprit:     "GET /api/users",
		Fingerprint: "fp-n1",
		Description: "SELECT * FROM users WHERE id = ?",
		Evidence:    map[string]any{"count": 6, "total_ms": int64(120), "span_ids": []string{"s1", "s2"}},
	}
}

func TestIssueServiceRecordCreatesThenIncrements(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newPerfProject(t, pool, "perf1")

	res, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if !res.Created {
		t.Fatalf("first Record: created = false, want true")
	}
	if res.Regression {
		t.Fatalf("first Record: regression = true, want false")
	}
	iss := res.Issue
	if iss.ID == 0 || iss.ProjectID != pid || iss.Count != 1 ||
		iss.Kind != trace.KindNPlusOne || iss.Status != "unresolved" || iss.SampleTraceID != "trace-a" {
		t.Fatalf("first Record: issue = %+v", iss)
	}
	var ev map[string]any
	if err := json.Unmarshal(iss.Evidence, &ev); err != nil {
		t.Fatalf("evidence: %v (raw %q)", err, iss.Evidence)
	}
	if ev["count"] != float64(6) {
		t.Errorf("evidence count = %v, want 6", ev["count"])
	}

	res2, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-b")
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if res2.Created {
		t.Fatalf("second Record: created = true, want false (повтор не должен алертить)")
	}
	again := res2.Issue
	if again.ID != iss.ID || again.Count != 2 {
		t.Fatalf("second Record: issue = %+v, want same id with count=2", again)
	}
	if again.LastSeen.Before(iss.LastSeen) {
		t.Errorf("last_seen went backwards: %v -> %v", iss.LastSeen, again.LastSeen)
	}
	if !again.FirstSeen.Equal(iss.FirstSeen) {
		t.Errorf("first_seen changed: %v -> %v", iss.FirstSeen, again.FirstSeen)
	}
}

func TestIssueServiceRecordResolvedRegression(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newPerfProject(t, pool, "perf2")

	first, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := svc.SetStatus(ctx, pid, first.Issue.ID, "resolved"); err != nil {
		t.Fatalf("SetStatus resolved: %v", err)
	}

	back, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-b")
	if err != nil {
		t.Fatalf("Record after resolve: %v", err)
	}
	if back.Created {
		t.Errorf("created = true, want false: регрессия — не новая проблема")
	}
	if !back.Regression {
		t.Error("regression = false: починенная и вернувшаяся проблема должна алертить")
	}
	if back.Issue.Status != "unresolved" {
		t.Errorf("status = %q, want unresolved (регрессия)", back.Issue.Status)
	}

	// Третье обнаружение — уже не регрессия: проблема и так unresolved.
	third, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-c")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if third.Created || third.Regression {
		t.Errorf("третье обнаружение: created=%v regression=%v, want false/false", third.Created, third.Regression)
	}
}

func TestIssueServiceRecordIgnoredStaysIgnored(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newPerfProject(t, pool, "perf3")

	first, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := svc.SetStatus(ctx, pid, first.Issue.ID, "ignored"); err != nil {
		t.Fatalf("SetStatus ignored: %v", err)
	}
	again, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-b")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if again.Issue.Status != "ignored" {
		t.Errorf("status = %q, want ignored", again.Issue.Status)
	}
	if again.Regression {
		t.Error("regression = true у ignored: заглушили осознанно, будить дежурного нельзя")
	}
}

func TestIssueServiceRecordSeparatesFindingsAndProjects(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid1 := newPerfProject(t, pool, "perfa")
	pid2 := newPerfProject(t, pool, "perfb")

	slow := trace.Finding{
		Kind:        trace.KindSlowDBQuery,
		Title:       "Медленный запрос: SELECT * FROM orders",
		Culprit:     "GET /api/users",
		Fingerprint: "fp-slow",
		Description: "SELECT * FROM orders",
		Evidence:    map[string]any{"count": 1},
	}

	// Две разные проблемы одного проекта — две строки.
	if r, err := svc.Record(ctx, pid1, nPlusOneFinding(), "t1"); err != nil || !r.Created {
		t.Fatalf("Record n+1: created=%v err=%v", r.Created, err)
	}
	if r, err := svc.Record(ctx, pid1, slow, "t1"); err != nil || !r.Created {
		t.Fatalf("Record slow: created=%v err=%v", r.Created, err)
	}
	// Тот же fingerprint в ДРУГОМ проекте — независимая строка.
	otherRes, err := svc.Record(ctx, pid2, nPlusOneFinding(), "t2")
	if err != nil || !otherRes.Created {
		t.Fatalf("Record other project: created=%v err=%v", otherRes.Created, err)
	}
	other := otherRes.Issue
	if other.Count != 1 {
		t.Errorf("other project count = %d, want 1", other.Count)
	}

	items, err := svc.List(ctx, pid1, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("List(pid1) = %d rows, want 2", len(items))
	}
	items2, err := svc.List(ctx, pid2, "", 10)
	if err != nil {
		t.Fatalf("List(pid2): %v", err)
	}
	if len(items2) != 1 || items2[0].ProjectID != pid2 || items2[0].ID != other.ID {
		t.Fatalf("List(pid2) = %+v, want 1 row of project %d", items2, pid2)
	}
}

func TestIssueServiceListFilterGetSetStatus(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newPerfProject(t, pool, "perf4")

	rec, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	iss := rec.Issue

	got, err := svc.Get(ctx, pid, iss.ID)
	if err != nil || got.ID != iss.ID || got.Fingerprint != "fp-n1" {
		t.Fatalf("Get = %+v err=%v", got, err)
	}
	// Чужой проект не видит проблему по её id (IDOR на /perf-issues/{id}).
	if _, err := svc.Get(ctx, newPerfProject(t, pool, "perf4-other"), iss.ID); !errors.Is(err, trace.ErrNotFound) {
		t.Errorf("Get(чужой проект) = %v, want ErrNotFound", err)
	}

	if err := svc.SetStatus(ctx, pid, iss.ID, "resolved"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	unresolved, err := svc.List(ctx, pid, "unresolved", 10)
	if err != nil {
		t.Fatalf("List unresolved: %v", err)
	}
	if len(unresolved) != 0 {
		t.Errorf("List(unresolved) = %d rows, want 0", len(unresolved))
	}
	resolved, err := svc.List(ctx, pid, "resolved", 10)
	if err != nil || len(resolved) != 1 || resolved[0].Status != "resolved" {
		t.Fatalf("List(resolved) = %+v err=%v", resolved, err)
	}

	if err := svc.SetStatus(ctx, pid, iss.ID, "bogus"); !errors.Is(err, trace.ErrInvalidStatus) {
		t.Errorf("SetStatus(bogus) = %v, want ErrInvalidStatus", err)
	}
	if err := svc.SetStatus(ctx, pid, iss.ID+9999, "resolved"); !errors.Is(err, trace.ErrNotFound) {
		t.Errorf("SetStatus(missing) = %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, pid, iss.ID+9999); !errors.Is(err, trace.ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

// Гонка двух первых обнаружений одного fingerprint: created=true имеет право
// вернуть РОВНО ОДИН из них — perf-алерты шлются по created, и второй true
// разбудил бы дежурного второй раз тем же самым.
//
// Гонка воспроизводится детерминированно: конкурент вставляет строку в открытой
// транзакции и не коммитит, Record упирается в уникальный индекс и ждёт. Его
// снимок (а значит, и CTE old) взят ДО коммита конкурента — старый код по этому
// снимку и решал, что проблема новая.
func TestIssueServiceRecordConcurrentFirstDetection(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newPerfProject(t, pool, "perfrace")
	f := nPlusOneFinding()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `INSERT INTO perf_issues
		(project_id, fingerprint, kind, title, culprit, count, sample_trace_id, evidence)
		VALUES ($1,$2,$3,$4,$5,1,$6,'{}')`,
		pid, f.Fingerprint, f.Kind, f.Title, f.Culprit, "trace-racer"); err != nil {
		t.Fatalf("racer insert: %v", err)
	}

	type recorded struct {
		res trace.RecordResult
		err error
	}
	done := make(chan recorded, 1)
	go func() {
		res, err := svc.Record(ctx, pid, f, "trace-b")
		done <- recorded{res, err}
	}()

	waitBlockedOnPerfIssues(t, pool)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("Record: %v", got.err)
	}
	if got.res.Created {
		t.Fatal("created = true у проигравшего гонку: алерт о первом обнаружении уйдёт дважды")
	}
	if got.res.Issue.Count != 2 {
		t.Errorf("count = %d, want 2", got.res.Issue.Count)
	}
}

// waitBlockedOnPerfIssues ждёт, пока Record упрётся в блокировку уникального
// индекса: без этого коммит конкурента мог бы обогнать его снимок и гонка не
// воспроизвелась бы.
func waitBlockedOnPerfIssues(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query ILIKE '%perf_issues%'`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Record так и не заблокировался на уникальном индексе")
}

// Повторное обнаружение НЕ переписывает evidence и sample_trace_id на каждом
// запросе: это jsonb, и переписывание горячей строки на каждой семплированной
// транзакции — лишняя запись WAL и лишний TOAST. Обновляются только count и
// last_seen; пример обновляется не чаще раза в час (см. perfSampleTTL).
func TestIssueServiceRecordKeepsSampleOnRepeat(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "sample-keep")

	first, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-first")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	second := nPlusOneFinding()
	second.Evidence = map[string]any{"count": 99, "span_ids": []string{"z9"}}
	rec, err := svc.Record(ctx, pid, second, "trace-second")
	if err != nil {
		t.Fatalf("Record repeat: %v", err)
	}

	if rec.Issue.Count != 2 {
		t.Errorf("count = %d, want 2", rec.Issue.Count)
	}
	if !rec.Issue.LastSeen.After(first.Issue.LastSeen) && !rec.Issue.LastSeen.Equal(first.Issue.LastSeen) {
		t.Errorf("last_seen поехал назад: %v -> %v", first.Issue.LastSeen, rec.Issue.LastSeen)
	}
	if rec.Issue.SampleTraceID != "trace-first" {
		t.Errorf("sample_trace_id = %q, want trace-first: свежий пример берётся не чаще раза в час",
			rec.Issue.SampleTraceID)
	}
	// jsonb хранится нормализованным (`{"count": 6, ...}`), поэтому сравниваем по
	// подстроке с пробелом после двоеточия.
	if !strings.Contains(string(rec.Issue.Evidence), `"count": 6`) {
		t.Errorf("evidence = %s, want первый образец (count=6)", rec.Issue.Evidence)
	}
}

// Осознанно заглушённая (ignored) проблема продолжает считаться, но НЕ всплывает
// наверх списка: last_seen у неё не двигается (List сортирует по last_seen DESC).
func TestIssueServiceIgnoredDoesNotResurface(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "ignored-quiet")

	first, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := svc.SetStatus(ctx, pid, first.Issue.ID, "ignored"); err != nil {
		t.Fatalf("SetStatus ignored: %v", err)
	}

	again, err := svc.Record(ctx, pid, nPlusOneFinding(), "trace-b")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if again.Issue.Count != 2 {
		t.Errorf("count = %d, want 2: считать заглушённую проблему мы не перестаём", again.Issue.Count)
	}
	if !again.Issue.LastSeen.Equal(first.Issue.LastSeen) {
		t.Errorf("last_seen = %v, want %v: заглушённая проблема не должна всплывать в списке",
			again.Issue.LastSeen, first.Issue.LastSeen)
	}
}

// Создание новых проблем ограничено MaxNewPerfIssuesPerHour на проект: без этого
// приложение с ObjectID/slug'ами в путях (и без шаблонизации маршрутов) льёт по
// новой строке perf_issues на КАЖДЫЙ запрос — таблица растёт без предела, а
// retention-задачи для неё нет. Уже существующие проблемы продолжают считаться:
// ограничивается создание, а не обнаружение.
func TestIssueServiceRecordCapsNewIssuesPerHour(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	pid := newPerfProject(t, pool, "issue-cap")
	other := newPerfProject(t, pool, "issue-cap-other")

	const attempts = 200
	created, suppressed := 0, 0
	for i := 0; i < attempts; i++ {
		f := nPlusOneFinding()
		f.Fingerprint = "fp-cap-" + strconv.Itoa(i)
		res, err := svc.Record(ctx, pid, f, "trace-"+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
		switch {
		case res.Created:
			created++
		case res.Suppressed:
			suppressed++
		default:
			t.Fatalf("Record %d: не создан и не подавлен: %+v", i, res)
		}
	}
	if created != trace.MaxNewPerfIssuesPerHour {
		t.Errorf("создано %d проблем, want %d", created, trace.MaxNewPerfIssuesPerHour)
	}
	if suppressed != attempts-trace.MaxNewPerfIssuesPerHour {
		t.Errorf("подавлено %d находок, want %d", suppressed, attempts-trace.MaxNewPerfIssuesPerHour)
	}

	var rows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_issues WHERE project_id = $1", pid).Scan(&rows); err != nil {
		t.Fatalf("count perf_issues: %v", err)
	}
	if rows != trace.MaxNewPerfIssuesPerHour {
		t.Fatalf("строк perf_issues = %d, want %d: кап не удержал рост таблицы",
			rows, trace.MaxNewPerfIssuesPerHour)
	}

	// Существующая проблема после выбранного капа продолжает инкрементиться.
	f := nPlusOneFinding()
	f.Fingerprint = "fp-cap-0"
	again, err := svc.Record(ctx, pid, f, "trace-again")
	if err != nil {
		t.Fatalf("Record существующей: %v", err)
	}
	if again.Created || again.Suppressed || again.Issue.Count != 2 {
		t.Fatalf("повтор существующей проблемы: %+v, want count=2 без created/suppressed", again)
	}

	// Кап у каждого проекта свой: выбранный лимит соседа не глушит.
	neighbour, err := svc.Record(ctx, other, nPlusOneFinding(), "trace-other")
	if err != nil {
		t.Fatalf("Record соседа: %v", err)
	}
	if !neighbour.Created {
		t.Fatalf("сосед: created=false, кап одного проекта не должен глушить другой: %+v", neighbour)
	}
}

// SetStatus обязан быть привязан к проекту: иначе участник чужой организации
// закрывает или глушит проблему по угаданному id (IDOR).
func TestIssueServiceSetStatusIsTenantScoped(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	victim := newPerfProject(t, pool, "victim")
	attacker := newPerfProject(t, pool, "attacker")

	rec, err := svc.Record(ctx, victim, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := svc.SetStatus(ctx, attacker, rec.Issue.ID, "resolved"); !errors.Is(err, trace.ErrNotFound) {
		t.Fatalf("SetStatus чужой проблемы: err = %v, want ErrNotFound", err)
	}
	got, err := svc.Get(ctx, victim, rec.Issue.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "unresolved" {
		t.Fatalf("status = %q, want unresolved: чужой SetStatus не должен был пройти", got.Status)
	}
	if err := svc.SetStatus(ctx, victim, rec.Issue.ID, "resolved"); err != nil {
		t.Fatalf("SetStatus своей проблемы: %v", err)
	}
}
