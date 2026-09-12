package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// ingest_signals — новая per-project таблица; ON DELETE CASCADE не должна оставлять сироту, как и
// остальные per-project таблицы (host_incidents, deployments, ...).
func TestMigrate0091IngestSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 90); err != nil {
		t.Fatalf("migrate to 90: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m91','M91',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m91','M91') RETURNING id", orgID)

	if err := db.MigratePGTo(dsn, 91); err != nil {
		t.Fatalf("migrate to 91: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO ingest_signals (project_id, kind, hits, last_seen_at)
		 VALUES ($1, 'key_invalid', 3, now())`, projectID); err != nil {
		t.Fatalf("insert ingest_signals: %v", err)
	}
	var hits int64
	mustScan(t, pool, &hits,
		"SELECT hits FROM ingest_signals WHERE project_id=$1 AND kind='key_invalid'", projectID)
	if hits != 3 {
		t.Fatalf("hits = %d, want 3", hits)
	}

	// PK(project_id,kind) — Recorder полагается на ON CONFLICT DO UPDATE ровно по этому ключу.
	if _, err := pool.Exec(ctx,
		`INSERT INTO ingest_signals (project_id, kind, hits, last_seen_at)
		 VALUES ($1, 'key_invalid', 1, now())`, projectID); err == nil {
		t.Fatal("повторная вставка (project_id, kind) прошла без ошибки — PRIMARY KEY не держит")
	}

	if _, err := pool.Exec(ctx, "DELETE FROM projects WHERE id=$1", projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var cnt int64
	mustScan(t, pool, &cnt, "SELECT count(*) FROM ingest_signals WHERE project_id=$1", projectID)
	if cnt != 0 {
		t.Fatalf("ingest_signals пережила удаление проекта: %d строк, want 0", cnt)
	}

	if err := db.MigratePGTo(dsn, 90); err != nil {
		t.Fatalf("migrate down to 90: %v", err)
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name='ingest_signals')").
		Scan(&exists); err != nil {
		t.Fatalf("check table after rollback: %v", err)
	}
	if exists {
		t.Fatal("ingest_signals должна исчезнуть после отката до 90")
	}
}
