package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Чистое добавление — цена ошибки ниже, но тест нужен: FK на projects(id) должен принимать существующие
// проекты, а UNIQUE(project_id, name) — реальный upsert-сценарий (ON CONFLICT).
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

	// Тот же upsert-сценарий, что host.Store.Upsert (internal/host/host.go).
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
