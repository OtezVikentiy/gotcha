package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки. На момент этой правки новейшая —
// 0071_deployments.up.sql (C5, задача 1).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0071CreatesDeployments — таблица deployments создаётся на базе,
// где уже есть организация и проект, принимает строку с FK на проект и уходит
// вместе с проектом при откате (DROP TABLE), не задев ни организацию, ни проект.
func TestMigrate0071CreatesDeployments(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 70); err != nil {
		t.Fatalf("migrate to 70: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m71', 'M71', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm71', 'M71') RETURNING id", orgID)

	if err := db.MigratePGTo(dsn, 71); err != nil {
		t.Fatalf("migrate to 71: %v", err)
	}

	// Таблица принимает строку с FK на существующий проект; DEFAULT-поля
	// заполняются без явных значений.
	var depID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO deployments (project_id, version, deployed_at) VALUES ($1, 'v1.0.0', $2) RETURNING id",
		projectID, time.Now().UTC()).Scan(&depID); err != nil {
		t.Fatalf("insert deployment после миграции: %v", err)
	}
	if depID == 0 {
		t.Fatal("deployment id = 0")
	}
	var env, url, changelog string
	if err := pool.QueryRow(ctx,
		"SELECT environment, url, changelog FROM deployments WHERE id = $1", depID).
		Scan(&env, &url, &changelog); err != nil {
		t.Fatalf("select deployment: %v", err)
	}
	if env != "" || url != "" || changelog != "" {
		t.Fatalf("DEFAULT-поля deployments = (%q, %q, %q), want пустые строки", env, url, changelog)
	}

	if err := db.MigratePGTo(dsn, 70); err != nil {
		t.Fatalf("migrate down to 70: %v", err)
	}
	var tableExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_name = 'deployments'
		)`).Scan(&tableExists); err != nil {
		t.Fatalf("check deployments table: %v", err)
	}
	if tableExists {
		t.Fatal("таблица deployments должна исчезнуть после отката 0071")
	}

	// Откат — DROP TABLE deployments, проект и организация обязаны уцелеть.
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM projects WHERE id = $1", projectID).Scan(&name); err != nil {
		t.Fatalf("project row must survive down-migration: %v", err)
	}
	if name != "M71" {
		t.Fatalf("project name = %q, want M71", name)
	}
}
