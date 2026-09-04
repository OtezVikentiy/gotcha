package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0090DepReleasedAt — K1-4 (аудит перед 1.0, волна 1):
// dep_released_at на host_incidents и incidents, nullable, без DEFAULT —
// момент снятия подавления зависимостью, от которого перезапускаются часы
// лесенки эскалации (см. докблок миграции). Проверяет: колонка появляется
// в обеих таблицах, у существующей строки — NULL, значение читается/пишется
// нормально, down убирает обе колонки без потери самих строк.
func TestMigrate0090DepReleasedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 89); err != nil {
		t.Fatalf("migrate to 89: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, hostID, monitorID, hostIncidentID, uptimeIncidentID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m90','M90',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m90','M90') RETURNING id", orgID)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1,'h90') RETURNING id", projectID)
	mustScan(t, pool, &monitorID,
		`INSERT INTO monitors (project_id, name, kind, interval_seconds)
		 VALUES ($1,'mon90','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &hostIncidentID,
		`INSERT INTO host_incidents (project_id, host_id, kind, status, current_value, peak_value)
		 VALUES ($1,$2,'silent','open',0,0) RETURNING id`, projectID, hostID)
	mustScan(t, pool, &uptimeIncidentID,
		`INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now()) RETURNING id`, monitorID)

	if err := db.MigratePGTo(dsn, 90); err != nil {
		t.Fatalf("migrate to 90: %v", err)
	}

	// 1) У существующих строк — NULL (нет DEFAULT, старые инциденты никогда
	// не были под подавлением зависимостью).
	var hostReleased, uptimeReleased *string
	if err := pool.QueryRow(ctx,
		"SELECT dep_released_at::text FROM host_incidents WHERE id=$1", hostIncidentID).Scan(&hostReleased); err != nil {
		t.Fatalf("select host_incidents.dep_released_at: %v", err)
	}
	if hostReleased != nil {
		t.Fatalf("host_incidents.dep_released_at = %v, want NULL", *hostReleased)
	}
	if err := pool.QueryRow(ctx,
		"SELECT dep_released_at::text FROM incidents WHERE id=$1", uptimeIncidentID).Scan(&uptimeReleased); err != nil {
		t.Fatalf("select incidents.dep_released_at: %v", err)
	}
	if uptimeReleased != nil {
		t.Fatalf("incidents.dep_released_at = %v, want NULL", *uptimeReleased)
	}

	// 2) Колонка пишется и читается.
	if _, err := pool.Exec(ctx,
		"UPDATE host_incidents SET dep_released_at = now() WHERE id=$1", hostIncidentID); err != nil {
		t.Fatalf("update host_incidents.dep_released_at: %v", err)
	}
	var afterWrite *string
	if err := pool.QueryRow(ctx,
		"SELECT dep_released_at::text FROM host_incidents WHERE id=$1", hostIncidentID).Scan(&afterWrite); err != nil {
		t.Fatalf("select после write: %v", err)
	}
	if afterWrite == nil {
		t.Fatal("host_incidents.dep_released_at осталась NULL после UPDATE")
	}

	// 3) Откат убирает обе колонки, строки переживают.
	if err := db.MigratePGTo(dsn, 89); err != nil {
		t.Fatalf("migrate down to 89: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='host_incidents' AND column_name='dep_released_at')").
		Scan(&exists); err != nil {
		t.Fatalf("check host_incidents column after rollback: %v", err)
	}
	if exists {
		t.Fatal("host_incidents.dep_released_at должна исчезнуть после отката до 89")
	}
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='incidents' AND column_name='dep_released_at')").
		Scan(&exists); err != nil {
		t.Fatalf("check incidents column after rollback: %v", err)
	}
	if exists {
		t.Fatal("incidents.dep_released_at должна исчезнуть после отката до 89")
	}

	var cnt int64
	mustScan(t, pool, &cnt, "SELECT count(*) FROM host_incidents WHERE id=$1", hostIncidentID)
	if cnt != 1 {
		t.Fatalf("host_incidents должен пережить откат колонки, count=%d, want 1", cnt)
	}
	mustScan(t, pool, &cnt, "SELECT count(*) FROM incidents WHERE id=$1", uptimeIncidentID)
	if cnt != 1 {
		t.Fatalf("incidents должен пережить откат колонки, count=%d, want 1", cnt)
	}
}
