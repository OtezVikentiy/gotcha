package issue_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newProject: прямые вставки — issue-пакет не зависит от org.
func newProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var userID, orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('i@example.com','x') RETURNING id").Scan(&userID); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('iss','Iss',1000000) RETURNING id").Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func TestUpsertLifecycle(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := issue.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)
	now := time.Now().UTC().Truncate(time.Millisecond)

	// Новый issue.
	r1, err := svc.Upsert(ctx, pid, "fp-1", "boom", "app.main", "error", "", now)
	if err != nil || !r1.New || r1.Regression || r1.IssueID == 0 {
		t.Fatalf("first upsert: %+v err=%v", r1, err)
	}

	// Повтор: не новый, не регрессия, times_seen растёт, last_seen двигается.
	later := now.Add(time.Minute)
	r2, err := svc.Upsert(ctx, pid, "fp-1", "boom", "app.main", "error", "", later)
	if err != nil || r2.New || r2.Regression || r2.IssueID != r1.IssueID {
		t.Fatalf("second upsert: %+v err=%v", r2, err)
	}
	var timesSeen int64
	var lastSeen time.Time
	var status string
	if err := pool.QueryRow(ctx,
		"SELECT times_seen, last_seen, status FROM issues WHERE id = $1", r1.IssueID).
		Scan(&timesSeen, &lastSeen, &status); err != nil {
		t.Fatalf("select: %v", err)
	}
	if timesSeen != 2 || !lastSeen.Equal(later) || status != "unresolved" {
		t.Fatalf("times_seen=%d last_seen=%v status=%s", timesSeen, lastSeen, status)
	}

	// Регрессия: resolved → снова пришло → unresolved + флаг.
	if _, err := pool.Exec(ctx, "UPDATE issues SET status='resolved' WHERE id=$1", r1.IssueID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	r3, err := svc.Upsert(ctx, pid, "fp-1", "boom", "app.main", "error", "", later.Add(time.Minute))
	if err != nil || r3.New || !r3.Regression {
		t.Fatalf("regression upsert: %+v err=%v", r3, err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM issues WHERE id=$1", r1.IssueID).Scan(&status); err != nil || status != "unresolved" {
		t.Fatalf("status after regression = %s err=%v", status, err)
	}

	// Ignored не реопенится и не считается регрессией.
	if _, err := pool.Exec(ctx, "UPDATE issues SET status='ignored' WHERE id=$1", r1.IssueID); err != nil {
		t.Fatalf("ignore: %v", err)
	}
	r4, err := svc.Upsert(ctx, pid, "fp-1", "boom", "app.main", "error", "", later.Add(2*time.Minute))
	if err != nil || r4.New || r4.Regression {
		t.Fatalf("ignored upsert: %+v err=%v", r4, err)
	}
	if err := pool.QueryRow(ctx, "SELECT status FROM issues WHERE id=$1", r1.IssueID).Scan(&status); err != nil || status != "ignored" {
		t.Fatalf("status after ignored-upsert = %s err=%v", status, err)
	}

	// Другой fingerprint — другой issue.
	r5, err := svc.Upsert(ctx, pid, "fp-2", "other", "", "warning", "", now)
	if err != nil || !r5.New || r5.IssueID == r1.IssueID {
		t.Fatalf("second fingerprint: %+v err=%v", r5, err)
	}
}
