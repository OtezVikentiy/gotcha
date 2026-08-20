package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0074HostThresholdOverrides — таблица host_threshold_overrides
// принимает частичный override (только disk, остальное NULL = наследовать),
// хост при удалении каскадно уносит override, откат снимает таблицу.
func TestMigrate0074HostThresholdOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 73); err != nil {
		t.Fatalf("migrate to 73: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var org, proj, hostID int64
	mustScan(t, pool, &org,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m74','M74',0) RETURNING id")
	mustScan(t, pool, &proj,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m74','M74') RETURNING id", org)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id,name) VALUES ($1,'h74') RETURNING id", proj)

	if err := db.MigratePGTo(dsn, 74); err != nil {
		t.Fatalf("migrate to 74: %v", err)
	}

	// Частичный override: только disk, остальное NULL (наследовать).
	if _, err := pool.Exec(ctx,
		"INSERT INTO host_threshold_overrides (host_id, disk_enabled, disk_threshold) VALUES ($1, true, 0.80)",
		hostID); err != nil {
		t.Fatalf("insert override: %v", err)
	}
	var de bool
	var dt float64
	var me *bool
	if err := pool.QueryRow(ctx,
		"SELECT disk_enabled, disk_threshold, memory_enabled FROM host_threshold_overrides WHERE host_id=$1", hostID).
		Scan(&de, &dt, &me); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !de || dt != 0.80 || me != nil {
		t.Fatalf("override = (%v,%v,%v), want (true,0.80,nil)", de, dt, me)
	}

	// CASCADE: удаление хоста снимает override.
	if _, err := pool.Exec(ctx, "DELETE FROM hosts WHERE id=$1", hostID); err != nil {
		t.Fatalf("del host: %v", err)
	}
	var cnt int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM host_threshold_overrides WHERE host_id=$1", hostID).Scan(&cnt); err != nil {
		t.Fatalf("count после удаления хоста: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("override не удалён каскадом: %d", cnt)
	}

	// Откат.
	if err := db.MigratePGTo(dsn, 73); err != nil {
		t.Fatalf("down to 73: %v", err)
	}
}
