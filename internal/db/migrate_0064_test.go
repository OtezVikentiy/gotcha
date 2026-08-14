package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки. На момент этой правки новейшая —
// 0064_hosts.up.sql.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0064HostsCreateThenDrop — 0064 добавляет таблицу hosts
// (реестр имён хостов, видевших события проекта, для оценки метрик по хосту).
// Чистое добавление, поэтому цена ошибки ниже, чем у деструктивных миграций,
// но тест на непустой базе всё равно нужен: FK на projects(id) обязан
// принимать существующие проекты, а UNIQUE(project_id, name) — обычный
// upsert-сценарий (тот же (project_id, name) дважды через ON CONFLICT).
// down проверяет, что таблица исчезает целиком.
func TestMigrate0064HostsCreateThenDrop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 63); err != nil {
		t.Fatalf("migrate to 63: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m64', 'M64', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm64', 'M64') RETURNING id", orgID)

	if err := db.MigratePGTo(dsn, 64); err != nil {
		t.Fatalf("migrate to 64: %v", err)
	}

	var hostID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'web-01') RETURNING id", projectID)

	// UNIQUE(project_id, name) + upsert-сценарий, каким его использует
	// host.Store.Upsert (см. internal/host/host.go).
	var reUpsertedID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (project_id, name) VALUES ($1, 'web-01')
		 ON CONFLICT (project_id, name) DO UPDATE SET last_seen = now()
		 RETURNING id`, projectID).Scan(&reUpsertedID); err != nil {
		t.Fatalf("upsert existing host: %v", err)
	}
	if reUpsertedID != hostID {
		t.Fatalf("upsert создал новую строку id=%d, want ту же id=%d (UNIQUE не работает)", reUpsertedID, hostID)
	}

	if err := db.MigratePGTo(dsn, 63); err != nil {
		t.Fatalf("migrate down to 63: %v", err)
	}

	var hostsTableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'hosts')`).
		Scan(&hostsTableExists); err != nil {
		t.Fatalf("check hosts table: %v", err)
	}
	if hostsTableExists {
		t.Fatal("таблица hosts должна исчезнуть после отката 0064")
	}
}
