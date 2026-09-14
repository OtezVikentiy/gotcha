package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// host.Store.ListActiveWithProject фильтрует по last_seen каждый тик — индекс обязан существовать.
func TestMigrate0095HostsLastSeenIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 94); err != nil {
		t.Fatalf("migrate to 94: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, hostID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m95','M95',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m95','M95') RETURNING id", orgID)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'web-1') RETURNING id", projectID)

	if err := db.MigratePGTo(dsn, 95); err != nil {
		t.Fatalf("migrate to 95: %v", err)
	}

	var indexed bool
	if err := pool.QueryRow(ctx,
		"SELECT indisvalid FROM pg_index WHERE indexrelid = 'hosts_last_seen_idx'::regclass").
		Scan(&indexed); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !indexed {
		t.Error("hosts_last_seen_idx не найден или невалиден после миграции до 95")
	}

	var hostCount int64
	mustScan(t, pool, &hostCount, "SELECT count(*) FROM hosts WHERE id = $1", hostID)
	if hostCount != 1 {
		t.Fatalf("hosts должны пережить миграцию индекса, count=%d, want 1", hostCount)
	}

	if err := db.MigratePGTo(dsn, 94); err != nil {
		t.Fatalf("migrate down to 94: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'hosts_last_seen_idx')").
		Scan(&exists); err != nil {
		t.Fatalf("check index after rollback: %v", err)
	}
	if exists {
		t.Error("hosts_last_seen_idx должен исчезнуть после отката до 94")
	}

	mustScan(t, pool, &hostCount, "SELECT count(*) FROM hosts WHERE id = $1", hostID)
	if hostCount != 1 {
		t.Fatalf("hosts должны пережить откат индекса, count=%d, want 1", hostCount)
	}
}
