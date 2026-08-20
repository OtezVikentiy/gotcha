package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0078AlertDependencies — таблица alert_dependencies (рёбра
// зависимостей узел→узел для подавления шторма алертов) и флаг
// suppressed_by_dep на host_incidents/incidents. Проверяет: дефолт флага на
// существующем инциденте, валидное ребро (родитель-монитор → ребёнок-хост),
// CHECK «ровно один родитель», CHECK «ровно один способ ребёнка», CHECK
// «label-пара scope/value вместе или никак», откат down-миграции.
func TestMigrate0078AlertDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 77); err != nil {
		t.Fatalf("migrate to 77: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var org, projectID, hostID, monitorID, incidentID int64
	mustScan(t, pool, &org,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m78','M78',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m78','M78') RETURNING id", org)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1,'h1') RETURNING id", projectID)
	mustScan(t, pool, &monitorID,
		`INSERT INTO monitors (project_id, name, kind, interval_seconds)
		 VALUES ($1,'m1','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &incidentID,
		`INSERT INTO host_incidents (project_id, host_id, kind, status)
		 VALUES ($1,$2,'silent','open') RETURNING id`, projectID, hostID)

	if err := db.MigratePGTo(dsn, 78); err != nil {
		t.Fatalf("migrate to 78: %v", err)
	}

	// 1) Дефолт флага на существующем инциденте = false.
	var flag bool
	if err := pool.QueryRow(ctx,
		"SELECT suppressed_by_dep FROM host_incidents WHERE id=$1", incidentID).Scan(&flag); err != nil {
		t.Fatalf("select suppressed_by_dep: %v", err)
	}
	if flag {
		t.Fatalf("suppressed_by_dep default = %v, want false", flag)
	}

	// 2) Валидное ребро вставляется (родитель-монитор → ребёнок-хост).
	if _, err := pool.Exec(ctx, `INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, monitorID, hostID); err != nil {
		t.Fatalf("insert valid edge: %v", err)
	}

	// 3) CHECK «ровно один родитель» отвергает два родителя.
	if _, err := pool.Exec(ctx, `INSERT INTO alert_dependencies (project_id, parent_host_id, parent_monitor_id, child_host_id)
		VALUES ($1,$2,$3,$4)`, projectID, hostID, monitorID, hostID); err == nil {
		t.Fatal("insert with two parents: want CHECK violation, got nil")
	}

	// 4) CHECK «ровно один способ ребёнка» отвергает ноль способов.
	if _, err := pool.Exec(ctx, `INSERT INTO alert_dependencies (project_id, parent_monitor_id) VALUES ($1,$2)`,
		projectID, monitorID); err == nil {
		t.Fatal("insert with no child: want CHECK violation, got nil")
	}

	// 5) Label-пара: scope без value отвергается.
	if _, err := pool.Exec(ctx, `INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_label_scope)
		VALUES ($1,$2,'env')`, projectID, monitorID); err == nil {
		t.Fatal("insert label scope without value: want CHECK violation, got nil")
	}

	// 6) Down откатывается.
	if err := db.MigratePGTo(dsn, 77); err != nil {
		t.Fatalf("migrate down to 77: %v", err)
	}
}
