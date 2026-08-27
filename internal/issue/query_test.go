package issue_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newOtherProject — второй, независимый проект в отдельной организации
// (newProject хардкодит email/slug и не годится для повторного вызова).
func newOtherProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('i2@example.com','x') RETURNING id").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('iss2','Iss2',1000000) RETURNING id").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api2','API2') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func TestListFilterAndStatus(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	r1, err := svc.Upsert(ctx, pid, "fp-1", "boom in worker", "app.worker", "error", "", t0)
	if err != nil {
		t.Fatalf("upsert fp-1: %v", err)
	}
	r2, err := svc.Upsert(ctx, pid, "fp-2", "slow query", "app.db", "warning", "", t0.Add(time.Second))
	if err != nil {
		t.Fatalf("upsert fp-2: %v", err)
	}
	r3, err := svc.Upsert(ctx, pid, "fp-3", "BOOM again", "", "debug", "", t0.Add(2*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-3: %v", err)
	}
	r4, err := svc.Upsert(ctx, pid, "fp-4", "fatal crash", "app.main", "fatal", "", t0.Add(3*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-4: %v", err)
	}
	// Повторные upsert поднимают times_seen и last_seen у fp-4 — самый частый и самый свежий.
	if _, err := svc.Upsert(ctx, pid, "fp-4", "fatal crash", "app.main", "fatal", "", t0.Add(4*time.Second)); err != nil {
		t.Fatalf("upsert fp-4 again: %v", err)
	}
	if _, err := svc.Upsert(ctx, pid, "fp-4", "fatal crash", "app.main", "fatal", "", t0.Add(5*time.Second)); err != nil {
		t.Fatalf("upsert fp-4 thrice: %v", err)
	}

	// List без фильтра: 4 issue, total 4, порядок по last_seen DESC (fp-4 первый).
	items, total, err := svc.List(ctx, pid, issue.Filter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 4 || len(items) != 4 {
		t.Fatalf("list default: total=%d len=%d", total, len(items))
	}
	if items[0].ID != r4.IssueID {
		t.Fatalf("list default order: first=%d want=%d", items[0].ID, r4.IssueID)
	}

	// Filter{Status:"resolved"} после SetStatus(fp-1) → 1.
	if err := svc.SetStatus(ctx, r1.IssueID, "resolved"); err != nil {
		t.Fatalf("set status resolved: %v", err)
	}
	items, total, err = svc.List(ctx, pid, issue.Filter{Status: "resolved"})
	if err != nil {
		t.Fatalf("list resolved: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != r1.IssueID {
		t.Fatalf("list resolved: total=%d len=%d items=%+v", total, len(items), items)
	}

	// Filter{Level:"warning"} → только fp-2.
	items, total, err = svc.List(ctx, pid, issue.Filter{Level: "warning"})
	if err != nil {
		t.Fatalf("list level warning: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != r2.IssueID {
		t.Fatalf("list level warning: total=%d len=%d items=%+v", total, len(items), items)
	}

	// Filter{Query:"boom"} — ILIKE регистронезависимо: fp-1 и fp-3.
	items, total, err = svc.List(ctx, pid, issue.Filter{Query: "boom"})
	if err != nil {
		t.Fatalf("list query boom: %v", err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("list query boom: total=%d len=%d items=%+v", total, len(items), items)
	}

	// Sort:"times_seen" — самый частый (fp-4) первым.
	items, total, err = svc.List(ctx, pid, issue.Filter{Sort: "times_seen"})
	if err != nil {
		t.Fatalf("list sort times_seen: %v", err)
	}
	if total != 4 || len(items) != 4 || items[0].ID != r4.IssueID {
		t.Fatalf("list sort times_seen: total=%d first=%d want=%d", total, items[0].ID, r4.IssueID)
	}
	if items[0].TimesSeen != 3 {
		t.Fatalf("fp-4 times_seen = %d want 3", items[0].TimesSeen)
	}

	// Пагинация PerPage=2 → 2 страницы по 2, total стабилен.
	page1, total1, err := svc.List(ctx, pid, issue.Filter{PerPage: 2, Page: 1})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	page2, total2, err := svc.List(ctx, pid, issue.Filter{PerPage: 2, Page: 2})
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	if total1 != 4 || total2 != 4 || len(page1) != 2 || len(page2) != 2 {
		t.Fatalf("pagination: total1=%d total2=%d len1=%d len2=%d", total1, total2, len(page1), len(page2))
	}
	seen := map[int64]bool{}
	for _, it := range append(page1, page2...) {
		seen[it.ID] = true
	}
	if len(seen) != 4 {
		t.Fatalf("pagination: expected 4 distinct issues across pages, got %d", len(seen))
	}

	// SetStatus: невалидный статус.
	if err := svc.SetStatus(ctx, r2.IssueID, "bogus"); !errors.Is(err, issue.ErrInvalidStatus) {
		t.Fatalf("set status invalid: err=%v want ErrInvalidStatus", err)
	}

	// SetStatus: несуществующий id.
	if err := svc.SetStatus(ctx, 999999999, "resolved"); !errors.Is(err, issue.ErrNotFound) {
		t.Fatalf("set status missing: err=%v want ErrNotFound", err)
	}

	// SetStatusBulk: только issues этого проекта.
	otherPID := newOtherProject(t, pool)
	otherR, err := svc.Upsert(ctx, otherPID, "fp-other", "other project issue", "", "error", "", t0)
	if err != nil {
		t.Fatalf("upsert other project: %v", err)
	}
	n, err := svc.SetStatusBulk(ctx, pid, []int64{r2.IssueID, r3.IssueID, otherR.IssueID}, "ignored")
	if err != nil {
		t.Fatalf("set status bulk: %v", err)
	}
	if n != 2 {
		t.Fatalf("set status bulk n=%d want 2", n)
	}
	got2, err := svc.Get(ctx, r2.IssueID)
	if err != nil || got2.Status != "ignored" {
		t.Fatalf("fp-2 after bulk: status=%s err=%v", got2.Status, err)
	}
	got3, err := svc.Get(ctx, r3.IssueID)
	if err != nil || got3.Status != "ignored" {
		t.Fatalf("fp-3 after bulk: status=%s err=%v", got3.Status, err)
	}
	gotOther, err := svc.Get(ctx, otherR.IssueID)
	if err != nil || gotOther.Status != "unresolved" {
		t.Fatalf("other project issue must stay untouched: status=%s err=%v", gotOther.Status, err)
	}

	// SetStatusBulk: невалидный статус.
	if _, err := svc.SetStatusBulk(ctx, pid, []int64{r4.IssueID}, "bogus"); !errors.Is(err, issue.ErrInvalidStatus) {
		t.Fatalf("set status bulk invalid: err=%v want ErrInvalidStatus", err)
	}

	// Get: несуществующий id.
	if _, err := svc.Get(ctx, 999999999); !errors.Is(err, issue.ErrNotFound) {
		t.Fatalf("get missing: err=%v want ErrNotFound", err)
	}

	// Assign: назначить пользователя и снять.
	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('assignee@example.com','x') RETURNING id").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := svc.Assign(ctx, r4.IssueID, &userID); err != nil {
		t.Fatalf("assign: %v", err)
	}
	got4, err := svc.Get(ctx, r4.IssueID)
	if err != nil || got4.AssigneeID == nil || *got4.AssigneeID != userID {
		t.Fatalf("assign result: assignee=%v err=%v", got4.AssigneeID, err)
	}
	if err := svc.Assign(ctx, r4.IssueID, nil); err != nil {
		t.Fatalf("unassign: %v", err)
	}
	got4, err = svc.Get(ctx, r4.IssueID)
	if err != nil || got4.AssigneeID != nil {
		t.Fatalf("unassign result: assignee=%v err=%v", got4.AssigneeID, err)
	}

	// Assign: несуществующий id.
	if err := svc.Assign(ctx, 999999999, nil); !errors.Is(err, issue.ErrNotFound) {
		t.Fatalf("assign missing: err=%v want ErrNotFound", err)
	}

	// ActiveSince: fp-4 was last touched at t0+5s, everything else at or
	// before t0+3s. A cutoff of t0+4s should return only fp-4.
	active, err := svc.ActiveSince(ctx, pid, t0.Add(4*time.Second))
	if err != nil {
		t.Fatalf("ActiveSince: %v", err)
	}
	if len(active) != 1 || active[0].ID != r4.IssueID {
		t.Fatalf("ActiveSince(t0+4s) = %+v, want only fp-4 (id=%d)", active, r4.IssueID)
	}

	// A cutoff before everything returns all 4, and other projects' issues
	// are excluded.
	activeAll, err := svc.ActiveSince(ctx, pid, t0.Add(-time.Minute))
	if err != nil {
		t.Fatalf("ActiveSince (all): %v", err)
	}
	if len(activeAll) != 4 {
		t.Fatalf("ActiveSince(all) = %d issues, want 4", len(activeAll))
	}
	for _, it := range activeAll {
		if it.ProjectID != pid {
			t.Fatalf("ActiveSince returned issue from another project: %+v", it)
		}
	}

	// A cutoff in the future returns nothing.
	activeNone, err := svc.ActiveSince(ctx, pid, t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("ActiveSince (future): %v", err)
	}
	if len(activeNone) != 0 {
		t.Fatalf("ActiveSince(future) = %d issues, want 0", len(activeNone))
	}

	// Test ILIKE wildcard escaping: "_" should not match any character when escaped.
	r5, err := svc.Upsert(ctx, pid, "fp-5", "worker_id crash", "", "error", "", t0.Add(6*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-5: %v", err)
	}
	_, err = svc.Upsert(ctx, pid, "fp-6", "workerXid crash", "", "error", "", t0.Add(7*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-6: %v", err)
	}

	// Filter{Query:"worker_id"} should return ONLY fp-5, not fp-6 (where _ was acting as wildcard).
	items, total, err = svc.List(ctx, pid, issue.Filter{Query: "worker_id"})
	if err != nil {
		t.Fatalf("list query worker_id: %v", err)
	}
	if total != 1 || len(items) != 1 {
		t.Fatalf("list query worker_id: total=%d len=%d want total=1 len=1", total, len(items))
	}
	if items[0].ID != r5.IssueID {
		t.Fatalf("list query worker_id: got ID=%d want=%d (should match only fp-5)", items[0].ID, r5.IssueID)
	}
}

// TestUpsertWritesIssueEnvironments проверяет, что Upsert денормализует
// environment в issue_environments (без дублей и без пустых строк).
func TestUpsertWritesIssueEnvironments(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	rProd, err := svc.Upsert(ctx, pid, "fp-env-prod", "prod issue", "app.prod", "error", "prod", t0)
	if err != nil {
		t.Fatalf("upsert prod: %v", err)
	}
	// Повторный upsert того же fingerprint/environment не плодит дубликат.
	if _, err := svc.Upsert(ctx, pid, "fp-env-prod", "prod issue", "app.prod", "error", "prod", t0.Add(time.Second)); err != nil {
		t.Fatalf("upsert prod again: %v", err)
	}
	var envCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM issue_environments WHERE issue_id = $1", rProd.IssueID).Scan(&envCount); err != nil {
		t.Fatalf("count issue_environments: %v", err)
	}
	if envCount != 1 {
		t.Fatalf("issue_environments rows for prod issue = %d, want 1 (no duplicates)", envCount)
	}

	rNoEnv, err := svc.Upsert(ctx, pid, "fp-env-none", "no env issue", "", "error", "", t0)
	if err != nil {
		t.Fatalf("upsert no env: %v", err)
	}
	var noEnvCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM issue_environments WHERE issue_id = $1", rNoEnv.IssueID).Scan(&noEnvCount); err != nil {
		t.Fatalf("count issue_environments (no env): %v", err)
	}
	if noEnvCount != 0 {
		t.Fatalf("issue_environments rows for no-env issue = %d, want 0 (empty environment not written)", noEnvCount)
	}
}

// TestFilterEnvironmentAndPeriod проверяет Filter.Environment (EXISTS по
// issue_environments) и Filter.Period (last_seen >= now() - whitelisted
// interval), включая игнорирование невалидного значения периода.
func TestFilterEnvironmentAndPeriod(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	rProd, err := svc.Upsert(ctx, pid, "fp-period-prod", "prod issue", "app.prod", "error", "prod", t0)
	if err != nil {
		t.Fatalf("upsert prod: %v", err)
	}
	rStaging, err := svc.Upsert(ctx, pid, "fp-period-staging", "staging issue", "app.staging", "error", "staging", t0)
	if err != nil {
		t.Fatalf("upsert staging: %v", err)
	}
	rNoEnv, err := svc.Upsert(ctx, pid, "fp-period-none", "no env issue", "", "error", "", t0)
	if err != nil {
		t.Fatalf("upsert no env: %v", err)
	}

	// Filter{Environment:"prod"} -> только prod issue.
	items, total, err := svc.List(ctx, pid, issue.Filter{Environment: "prod"})
	if err != nil {
		t.Fatalf("list environment prod: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != rProd.IssueID {
		t.Fatalf("list environment prod: total=%d len=%d items=%+v", total, len(items), items)
	}

	// Filter{Environment:"staging"} -> только staging issue.
	items, total, err = svc.List(ctx, pid, issue.Filter{Environment: "staging"})
	if err != nil {
		t.Fatalf("list environment staging: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != rStaging.IssueID {
		t.Fatalf("list environment staging: total=%d len=%d items=%+v", total, len(items), items)
	}

	// Подкручиваем last_seen staging issue на 2 суток назад напрямую.
	if _, err := pool.Exec(ctx, "UPDATE issues SET last_seen = $1 WHERE id = $2", t0.Add(-48*time.Hour), rStaging.IssueID); err != nil {
		t.Fatalf("backdate staging last_seen: %v", err)
	}

	// Граница Since отсекает staging (last_seen 48h назад), оставляет prod и no-env.
	items, total, err = svc.List(ctx, pid, issue.Filter{Since: t0.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("list since 24h: %v", err)
	}
	if total != 2 {
		t.Fatalf("list since 24h: total=%d want 2", total)
	}
	for _, it := range items {
		if it.ID == rStaging.IssueID {
			t.Fatalf("list since 24h: leaked backdated staging issue: %+v", items)
		}
	}

	// Произвольное окно: только то, что попало между границами. Раньше такой
	// фильтр в списке проблем был недоступен вовсе — период задавался строкой
	// из белого списка (24h|7d|30d).
	items, total, err = svc.List(ctx, pid, issue.Filter{
		Since: t0.Add(-72 * time.Hour),
		Until: t0.Add(-36 * time.Hour),
	})
	if err != nil {
		t.Fatalf("list custom window: %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != rStaging.IssueID {
		t.Fatalf("list custom window: total=%d items=%+v, want только backdated staging", total, items)
	}

	// Без границ — все три.
	items, total, err = svc.List(ctx, pid, issue.Filter{})
	if err != nil {
		t.Fatalf("list unbounded: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("list unbounded: total=%d len=%d, want 3", total, len(items))
	}
	_ = rNoEnv
}

// TestEnvironmentsListAndAssigneeEmail проверяет Service.Environments
// (отсортированный уникальный список) и Issue.AssigneeEmail (заполняется
// List/Get, пуст без назначения, содержит email после Assign).
func TestEnvironmentsListAndAssigneeEmail(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	rProd, err := svc.Upsert(ctx, pid, "fp-envs-prod", "prod issue", "app.prod", "error", "prod", t0)
	if err != nil {
		t.Fatalf("upsert prod: %v", err)
	}
	if _, err := svc.Upsert(ctx, pid, "fp-envs-prod2", "prod issue 2", "app.prod", "error", "prod", t0); err != nil {
		t.Fatalf("upsert prod2: %v", err)
	}
	if _, err := svc.Upsert(ctx, pid, "fp-envs-staging", "staging issue", "app.staging", "error", "staging", t0); err != nil {
		t.Fatalf("upsert staging: %v", err)
	}

	envs, err := svc.Environments(ctx, pid)
	if err != nil {
		t.Fatalf("environments: %v", err)
	}
	want := []string{"prod", "staging"}
	if !reflect.DeepEqual(envs, want) {
		t.Fatalf("environments = %v, want %v", envs, want)
	}

	// AssigneeEmail пуст до назначения (проверяем и через Get, и через List).
	got, err := svc.Get(ctx, rProd.IssueID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AssigneeEmail != "" {
		t.Fatalf("AssigneeEmail before assign = %q, want empty", got.AssigneeEmail)
	}

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('assignee-email@example.com','x') RETURNING id").Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := svc.Assign(ctx, rProd.IssueID, &userID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	got, err = svc.Get(ctx, rProd.IssueID)
	if err != nil {
		t.Fatalf("get after assign: %v", err)
	}
	if got.AssigneeEmail != "assignee-email@example.com" {
		t.Fatalf("AssigneeEmail after assign (Get) = %q, want assignee-email@example.com", got.AssigneeEmail)
	}

	items, _, err := svc.List(ctx, pid, issue.Filter{Environment: "prod"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, it := range items {
		if it.ID == rProd.IssueID {
			found = true
			if it.AssigneeEmail != "assignee-email@example.com" {
				t.Fatalf("AssigneeEmail after assign (List) = %q, want assignee-email@example.com", it.AssigneeEmail)
			}
		}
	}
	if !found {
		t.Fatalf("prod issue not found in list result")
	}
}

// TestIssueListSameResultWithoutWindowCount: total ушёл из основного запроса
// (count(*) OVER() → отдельный count(*) без JOIN/ORDER BY, см. List), поэтому
// нужно доказать, что список, его порядок и total не изменились — оптимизация,
// поменявшая выдачу, это дефект, а не оптимизация.
//
// n кратно perPage (30 issue при perPage=10 → ровно три полные страницы) —
// специально, а не 25: так следующая, четвёртая страница даёт offset,
// РОВНО совпадающий с total (30 == 30), а не просто больший — граница
// `offset >= total` в List иначе проверялась бы только строгим неравенством,
// а именно на равенстве такие правки чаще всего и ломаются при переработке.
func TestIssueListSameResultWithoutWindowCount(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	const n = 30
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		fp := fmt.Sprintf("fp-window-%02d", i)
		r, err := svc.Upsert(ctx, pid, fp, fp, "", "error", "", t0.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatalf("upsert %s: %v", fp, err)
		}
		ids[i] = r.IssueID
	}
	// last_seen DESC: последний засеянный (i=n-1, самый поздний t0+29s) первый.
	wantOrder := make([]int64, n)
	for i := 0; i < n; i++ {
		wantOrder[i] = ids[n-1-i]
	}

	const perPage = 10
	const lastPage = n / perPage // 3 полные страницы, без остатка
	var gotOrder []int64
	var totals []int64
	for page := 1; page <= lastPage; page++ {
		items, total, err := svc.List(ctx, pid, issue.Filter{PerPage: perPage, Page: page})
		if err != nil {
			t.Fatalf("list page %d: %v", page, err)
		}
		totals = append(totals, total)
		for _, it := range items {
			gotOrder = append(gotOrder, it.ID)
		}
		if len(items) != perPage {
			t.Fatalf("page %d: len=%d, want %d", page, len(items), perPage)
		}
	}

	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Fatalf("порядок по %d страницам не совпал с ожидаемым:\ngot:  %v\nwant: %v", lastPage, gotOrder, wantOrder)
	}
	for i, total := range totals {
		if total != n {
			t.Fatalf("страница %d: total=%d, want %d (total должен быть одинаковым на каждой странице)", i+1, total, n)
		}
	}

	// Точная граница: страница lastPage+1 даёт offset = lastPage*perPage = n —
	// РОВНО равно total, не больше. Ожидаем тот же total=0/items=nil, что и
	// строго за пределами данных: сравнение в List — `offset >= total`, и
	// именно случай равенства здесь и проверяется, а не только «больше».
	exactBoundary, total, err := svc.List(ctx, pid, issue.Filter{PerPage: perPage, Page: lastPage + 1})
	if err != nil {
		t.Fatalf("list exact boundary: %v", err)
	}
	if total != 0 || len(exactBoundary) != 0 {
		t.Fatalf("страница на точной границе (offset==total==%d): total=%d len=%d, want 0 и 0", n, total, len(exactBoundary))
	}

	// Страница за пределами данных: total=0, items=nil — тот же результат, что
	// раньше давал count(*) OVER() в одном запросе с LIMIT/OFFSET (если
	// смещение выходит за пределы набора, строк не возвращается вовсе, а
	// значит total, который заполнялся сканированием строки, оставался нулём).
	// Шаблон пагинации (issues.templ, pagerPrev) читает total<=0 как «страницы
	// нет — веди на первую», поэтому это поведение обязано быть сохранено
	// буквально, а не только «в целом эквивалентно».
	outOfRange, total, err := svc.List(ctx, pid, issue.Filter{PerPage: perPage, Page: 100})
	if err != nil {
		t.Fatalf("list out of range: %v", err)
	}
	if total != 0 || len(outOfRange) != 0 {
		t.Fatalf("страница за пределами данных: total=%d len=%d, want 0 и 0", total, len(outOfRange))
	}
}

// mustUpsert — обёртка Upsert для тестов StreamForExport, где сам факт
// создания группы важен, а результат upsert (New/Regression) — нет.
func mustUpsert(t *testing.T, svc *issue.Service, projectID int64, fingerprint string, seenAt time.Time) int64 {
	t.Helper()
	r, err := svc.Upsert(context.Background(), projectID, fingerprint, "t", "c", "error", "", seenAt)
	if err != nil {
		t.Fatalf("upsert %s: %v", fingerprint, err)
	}
	return r.IssueID
}

// TestStreamForExportNoGapsOnEqualLastSeen — худший случай для курсора
// (last_seen, id): все группы с ОДИНАКОВЫМ last_seen. Без id вторым ключом
// курсора сравнение "< curSeen" отбросило бы все строки с тем же last_seen,
// что и последняя строка предыдущей страницы, — целую пачку сразу.
// n больше exportPageSize (500 в internal/issue/query.go), иначе весь набор
// читается одной страницей и граница вообще не проверяется.
func TestStreamForExportNoGapsOnEqualLastSeen(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)

	same := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	const n = 600
	for i := 0; i < n; i++ {
		mustUpsert(t, svc, pid, fmt.Sprintf("fp-%03d", i), same)
	}

	seen := map[int64]int{}
	err := svc.StreamForExport(ctx, pid, issue.Filter{Status: "unresolved"}, func(it issue.Issue) error {
		seen[it.ID]++
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if len(seen) != n {
		t.Fatalf("выгружено %d групп из %d — курсор теряет строки на стыке страниц", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("группа %d выдана %d раз", id, c)
		}
	}
}

// TestStreamForExportStopsOnCallbackError — потолок строк реализуется
// остановкой обхода снаружи (в источнике выгрузки): StreamForExport обязан
// прекратить читать страницы, как только колбэк вернул ошибку, а не
// дочитать текущую выборку до конца.
func TestStreamForExportStopsOnCallbackError(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)
	for i := 0; i < 10; i++ {
		mustUpsert(t, svc, pid, fmt.Sprintf("fp-%d", i), time.Now().UTC())
	}
	stop := errors.New("хватит")
	got := 0
	err := svc.StreamForExport(ctx, pid, issue.Filter{}, func(issue.Issue) error {
		got++
		if got == 3 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("StreamForExport вернул %v, ожидали ошибку колбэка", err)
	}
	if got != 3 {
		t.Errorf("обход продолжился после отказа: %d строк", got)
	}
}

// TestStreamForExportIgnoresOtherProject — фильтр keyset-обхода обязан
// содержать project_id, как и обычный List: без него курсор по (last_seen,id)
// свободно перетекал бы в чужие группы, если их id/last_seen попадают в тот
// же диапазон.
func TestStreamForExportIgnoresOtherProject(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)
	other := newOtherProject(t, pool)

	now := time.Now().UTC()
	want := mustUpsert(t, svc, pid, "own", now)
	mustUpsert(t, svc, other, "foreign", now.Add(time.Second))

	var got []int64
	if err := svc.StreamForExport(ctx, pid, issue.Filter{}, func(it issue.Issue) error {
		got = append(got, it.ID)
		return nil
	}); err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("StreamForExport(pid) = %v, want [%d] — чужая группа утекла", got, want)
	}
}

// TestStreamForExportSurvivesMutationBetweenPages — вставка и удаление строк
// между чтением страниц не должны приводить ни к дублю, ни к панике/ошибке.
// Группы заведены с РАЗНЫМ last_seen (по возрастанию i), поэтому при
// ORDER BY last_seen DESC первая страница (500 строк) захватывает группы с
// НАИБОЛЬШИМ last_seen (i от 599 до 100), вторая — оставшиеся (i от 99 до 0).
// Прямо на границе (после 500-й отданной строки, i=100) тест:
//   - удаляет ещё не прочитанную группу (i=0, самый ранний last_seen —
//     будет прочитана последней) — она не должна попасть в выдачу;
//   - вставляет новую группу с last_seen МЕНЬШЕ курсора (i.е. в ещё не
//     прочитанной части набора) — она обязана попасть в выдачу ровно один раз.
func TestStreamForExportSurvivesMutationBetweenPages(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)

	const n = 600
	base := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	ids := make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = mustUpsert(t, svc, pid, fmt.Sprintf("fp-%04d", i), base.Add(time.Duration(i)*time.Second))
	}
	deletedID := ids[0] // last_seen самый ранний — читается последним

	var insertedID int64
	count := 0
	seen := map[int64]int{}
	err := svc.StreamForExport(ctx, pid, issue.Filter{}, func(it issue.Issue) error {
		seen[it.ID]++
		count++
		if count == 500 {
			if _, err := pool.Exec(ctx, "DELETE FROM issues WHERE id = $1", deletedID); err != nil {
				t.Fatalf("delete between pages: %v", err)
			}
			// last_seen раньше самой ранней уже заведённой группы — попадёт
			// в хвост второй страницы, обгоняя оставшиеся i=99..0.
			insertedID = mustUpsert(t, svc, pid, "fp-inserted-late", base.Add(-time.Hour))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("StreamForExport: %v", err)
	}
	if seen[deletedID] != 0 {
		t.Errorf("удалённая между страницами группа попала в выдачу")
	}
	if seen[insertedID] != 1 {
		t.Errorf("вставленная между страницами группа выдана %d раз, want 1", seen[insertedID])
	}
	for id, c := range seen {
		if c > 1 {
			t.Errorf("группа %d выдана %d раз", id, c)
		}
	}
	wantTotal := n - 1 + 1 // минус удалённая, плюс вставленная
	if len(seen) != wantTotal {
		t.Fatalf("выгружено %d групп, want %d", len(seen), wantTotal)
	}
}

// TestIDsForFilterReportsOverflow — упор в потолок id групп обязан дать
// отказ (overflow=true), а не тихую обрезку: источник выгрузки событий
// (kind=events, область «проект с фильтрами») не может сказать пользователю,
// какие именно группы выпали бы из списка, поэтому решение — отказать и
// попросить сузить фильтр (§8 спеки экспорта), а не отдать неполный список.
func TestIDsForFilterReportsOverflow(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)
	for i := 0; i < 7; i++ {
		mustUpsert(t, svc, pid, fmt.Sprintf("fp-of-%d", i), time.Now().UTC())
	}

	ids, overflow, err := svc.IDsForFilter(ctx, pid, issue.Filter{}, 5)
	if err != nil {
		t.Fatalf("IDsForFilter: %v", err)
	}
	if !overflow {
		t.Fatal("потолок пройден молча — выгрузка окажется неполной без предупреждения")
	}
	if len(ids) != 5 {
		t.Errorf("вернулось %d id при потолке 5, want 5", len(ids))
	}
}

// TestIDsForFilterNoOverflowReturnsExactMatch — без упора в потолок
// возвращается ровно то, что подходит под фильтр (не хвост списка).
func TestIDsForFilterNoOverflowReturnsExactMatch(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)
	now := time.Now().UTC()
	want := mustUpsert(t, svc, pid, "fp-match", now)
	if err := svc.SetStatus(ctx, want, "resolved"); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	mustUpsert(t, svc, pid, "fp-nomatch", now.Add(time.Second)) // остаётся unresolved

	ids, overflow, err := svc.IDsForFilter(ctx, pid, issue.Filter{Status: "resolved"}, 100)
	if err != nil {
		t.Fatalf("IDsForFilter: %v", err)
	}
	if overflow {
		t.Fatal("overflow = true при наборе меньше потолка")
	}
	if len(ids) != 1 || ids[0] != want {
		t.Fatalf("IDsForFilter = %v, want [%d]", ids, want)
	}
}

// TestIDsForFilterIsolatedByProject — чужой project_id не должен утекать в
// список ни при каких параметрах фильтра: id групп уходят прямо в
// ClickHouse-фильтр IN (…), и утечка здесь означала бы утечку чужих событий.
func TestIDsForFilterIsolatedByProject(t *testing.T) {
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	pid := newProject(t, pool)
	other := newOtherProject(t, pool)
	now := time.Now().UTC()
	want := mustUpsert(t, svc, pid, "own", now)
	mustUpsert(t, svc, other, "foreign", now.Add(time.Second))

	ids, overflow, err := svc.IDsForFilter(ctx, pid, issue.Filter{}, 100)
	if err != nil {
		t.Fatalf("IDsForFilter: %v", err)
	}
	if overflow {
		t.Fatal("overflow = true неожиданно")
	}
	if len(ids) != 1 || ids[0] != want {
		t.Fatalf("IDsForFilter(pid) = %v, want [%d] — чужая группа утекла", ids, want)
	}
}
