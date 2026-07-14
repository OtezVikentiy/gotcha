package issue_test

import (
	"context"
	"errors"
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

	r1, err := svc.Upsert(ctx, pid, "fp-1", "boom in worker", "app.worker", "error", t0)
	if err != nil {
		t.Fatalf("upsert fp-1: %v", err)
	}
	r2, err := svc.Upsert(ctx, pid, "fp-2", "slow query", "app.db", "warning", t0.Add(time.Second))
	if err != nil {
		t.Fatalf("upsert fp-2: %v", err)
	}
	r3, err := svc.Upsert(ctx, pid, "fp-3", "BOOM again", "", "debug", t0.Add(2*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-3: %v", err)
	}
	r4, err := svc.Upsert(ctx, pid, "fp-4", "fatal crash", "app.main", "fatal", t0.Add(3*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-4: %v", err)
	}
	// Повторные upsert поднимают times_seen и last_seen у fp-4 — самый частый и самый свежий.
	if _, err := svc.Upsert(ctx, pid, "fp-4", "fatal crash", "app.main", "fatal", t0.Add(4*time.Second)); err != nil {
		t.Fatalf("upsert fp-4 again: %v", err)
	}
	if _, err := svc.Upsert(ctx, pid, "fp-4", "fatal crash", "app.main", "fatal", t0.Add(5*time.Second)); err != nil {
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
	otherR, err := svc.Upsert(ctx, otherPID, "fp-other", "other project issue", "", "error", t0)
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

	// Test ILIKE wildcard escaping: "_" should not match any character when escaped.
	r5, err := svc.Upsert(ctx, pid, "fp-5", "worker_id crash", "", "error", t0.Add(6*time.Second))
	if err != nil {
		t.Fatalf("upsert fp-5: %v", err)
	}
	_, err = svc.Upsert(ctx, pid, "fp-6", "workerXid crash", "", "error", t0.Add(7*time.Second))
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
