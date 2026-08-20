package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0076MaintenanceAllSources — in_maintenance добавляется на существующую
// (непустую) таблицу host_incidents с default false, а CHECK maintenance_windows
// после смягчения пропускает бессрочное разовое окно (ends_at NULL). Откат должен
// пройти без ошибки — down сначала удаляет бессрочные окна, потом возвращает строгий
// CHECK (иначе ADD CONSTRAINT валидирует существующие строки и падает).
func TestMigrate0076MaintenanceAllSources(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 75); err != nil {
		t.Fatalf("migrate to 75: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var org, proj int64
	mustScan(t, pool, &org,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m76','M76',0) RETURNING id")
	mustScan(t, pool, &proj,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m76','M76') RETURNING id", org)

	// host_incident существует до миграции 76 — проверяет ADD COLUMN на непустой таблице.
	var hostID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id,name) VALUES ($1,'h76') RETURNING id", proj)
	mustExec(t, pool,
		"INSERT INTO host_incidents (project_id,host_id,kind,status) VALUES ($1,$2,'disk','open')",
		proj, hostID)

	if err := db.MigratePGTo(dsn, 76); err != nil {
		t.Fatalf("migrate to 76: %v", err)
	}

	var im bool
	if err := pool.QueryRow(ctx,
		"SELECT in_maintenance FROM host_incidents WHERE host_id=$1", hostID).Scan(&im); err != nil {
		t.Fatalf("select: %v", err)
	}
	if im {
		t.Fatalf("existing incident in_maintenance = true, want false (default)")
	}

	// Бессрочное окно (ends_at NULL) проходит новый CHECK.
	if _, err := pool.Exec(ctx,
		"INSERT INTO maintenance_windows (project_id,name,weekly,starts_at) VALUES ($1,'inf',false,now())",
		proj); err != nil {
		t.Fatalf("indefinite window insert rejected: %v", err)
	}

	// Откат — BLOCKER-2: не должен упасть на бессрочном окне, оставшемся в таблице.
	if err := db.MigratePGTo(dsn, 75); err != nil {
		t.Fatalf("down to 75: %v", err)
	}
}
