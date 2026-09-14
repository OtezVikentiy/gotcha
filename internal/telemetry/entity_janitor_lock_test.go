package telemetry

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// package telemetry, не telemetry_test: тест держит advisory-лок по
// entityJanitorLockID напрямую, символ неэкспортируемый.

func newEntityLockTestProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := time.Now().UnixNano()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Lock',1000000) RETURNING id",
		fmt.Sprintf("janitor-lock-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,'api','API') RETURNING id", orgID).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

// EntityJanitor.Tick клеймит pg_try_advisory_lock(entityJanitorLockID) — при
// занятом локе проход обязан уступить другой реплике, а не удалять параллельно.
func TestEntityJanitorSkipsWhenAnotherInstanceHoldsLock(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newEntityLockTestProject(t, pool)

	var issueID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO issues (project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen)
		 VALUES ($1,'lock-fp','boom','app.go','error','unresolved',$2,$2,1) RETURNING id`,
		pid, time.Now().UTC().Add(-48*time.Hour)).Scan(&issueID); err != nil {
		t.Fatalf("insert issue: %v", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(entityJanitorLockID)).Scan(&locked); err != nil || !locked {
		t.Fatalf("подготовка занятого лока: locked=%v err=%v", locked, err)
	}
	defer conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(entityJanitorLockID))

	jan := &EntityJanitor{Pool: pool, Retention: Retentions{Events: 24 * time.Hour}}
	n, err := jan.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Errorf("purged = %d, want 0: лок держит другая реплика", n)
	}

	var exists bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM issues WHERE id = $1)", issueID).Scan(&exists); err != nil {
		t.Fatalf("issue exists: %v", err)
	}
	if !exists {
		t.Error("просроченный issue убран, пока лок держала другая реплика")
	}
}
