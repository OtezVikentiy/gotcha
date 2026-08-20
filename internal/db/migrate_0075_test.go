package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0075HostGroupThresholds — групповое правило порогов по метке
// (scope/label) для проекта, каскадное удаление от projects, откат снимает
// таблицу.
func TestMigrate0075HostGroupThresholds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 74); err != nil {
		t.Fatalf("migrate to 74: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var org, proj int64
	mustScan(t, pool, &org,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m75','M75',0) RETURNING id")
	mustScan(t, pool, &proj,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m75','M75') RETURNING id", org)

	if err := db.MigratePGTo(dsn, 75); err != nil {
		t.Fatalf("migrate to 75: %v", err)
	}

	// Групповое правило: роль web, только load.
	if _, err := pool.Exec(ctx,
		"INSERT INTO host_group_thresholds (project_id, scope, label, load_enabled, load_threshold) VALUES ($1, 'role', 'web', true, 4.0)",
		proj); err != nil {
		t.Fatalf("insert group threshold: %v", err)
	}
	var scope, label string
	var le bool
	var lt float64
	var de *bool
	if err := pool.QueryRow(ctx,
		"SELECT scope, label, load_enabled, load_threshold, disk_enabled FROM host_group_thresholds WHERE project_id=$1 AND scope='role' AND label='web'", proj).
		Scan(&scope, &label, &le, &lt, &de); err != nil {
		t.Fatalf("select: %v", err)
	}
	if scope != "role" || label != "web" || !le || lt != 4.0 || de != nil {
		t.Fatalf("group threshold = (%v,%v,%v,%v,%v), want (role,web,true,4,nil)", scope, label, le, lt, de)
	}

	// CASCADE: удаление проекта снимает групповое правило.
	if _, err := pool.Exec(ctx, "DELETE FROM projects WHERE id=$1", proj); err != nil {
		t.Fatalf("del project: %v", err)
	}
	var cnt int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM host_group_thresholds WHERE project_id=$1", proj).Scan(&cnt); err != nil {
		t.Fatalf("count после удаления проекта: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("группа не удалена каскадом: %d", cnt)
	}

	// Откат.
	if err := db.MigratePGTo(dsn, 74); err != nil {
		t.Fatalf("down to 74: %v", err)
	}
}
