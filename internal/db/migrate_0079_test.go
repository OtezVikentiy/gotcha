package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0079IncidentGroups — таблица incident_groups (группы
// коррелированных алертов с корнем host/uptime) и колонка group_id на четырёх
// таблицах инцидентов. Проверяет: NULL-дефолт group_id на существующем
// инциденте, валидную группу, CHECK root_source, UNIQUE (root_source,
// root_incident_id), намеренное отсутствие FK на group_id, каскад удаления
// проекта, откат down-миграции.
func TestMigrate0079IncidentGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 78); err != nil {
		t.Fatalf("migrate to 78: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var org, projectID, hostID, incidentID int64
	mustScan(t, pool, &org,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m79','M79',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m79','M79') RETURNING id", org)
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1,'h1') RETURNING id", projectID)
	mustScan(t, pool, &incidentID,
		`INSERT INTO host_incidents (project_id, host_id, kind, status)
		 VALUES ($1,$2,'silent','open') RETURNING id`, projectID, hostID)

	if err := db.MigratePGTo(dsn, 79); err != nil {
		t.Fatalf("migrate to 79: %v", err)
	}

	// 1) group_id на существующем инциденте = NULL.
	var groupID *int64
	if err := pool.QueryRow(ctx,
		"SELECT group_id FROM host_incidents WHERE id=$1", incidentID).Scan(&groupID); err != nil {
		t.Fatalf("select group_id: %v", err)
	}
	if groupID != nil {
		t.Fatalf("group_id after migration = %v, want NULL", *groupID)
	}

	// 2) Валидная группа вставляется.
	var gid int64
	mustScan(t, pool, &gid,
		`INSERT INTO incident_groups (project_id, root_source, root_incident_id, root_node_kind, root_node_id)
		 VALUES ($1,'host',$2,'host',$3) RETURNING id`, projectID, incidentID, hostID)

	// 3) CHECK root_source отвергает неизвестный источник.
	if _, err := pool.Exec(ctx,
		`INSERT INTO incident_groups (project_id, root_source, root_incident_id, root_node_kind, root_node_id)
		 VALUES ($1,'metric',$2,'host',$3)`, projectID, incidentID, hostID); err == nil {
		t.Fatal("insert root_source='metric': want CHECK violation, got nil")
	}

	// 4) UNIQUE (root_source, root_incident_id) отвергает дубль корня.
	if _, err := pool.Exec(ctx,
		`INSERT INTO incident_groups (project_id, root_source, root_incident_id, root_node_kind, root_node_id)
		 VALUES ($1,'host',$2,'host',$3)`, projectID, incidentID, hostID); err == nil {
		t.Fatal("insert duplicate (root_source, root_incident_id): want UNIQUE violation, got nil")
	}

	// 5) group_id проставляется, и FK на него намеренно нет: несуществующая
	// группа тоже принимается (лог историчен, группа переживает ретеншен —
	// прецедент incident_escalations, 0077).
	if _, err := pool.Exec(ctx,
		"UPDATE host_incidents SET group_id=$1 WHERE id=$2", gid, incidentID); err != nil {
		t.Fatalf("set group_id: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE host_incidents SET group_id=$1 WHERE id=$2", gid+1000000, incidentID); err != nil {
		t.Fatalf("set dangling group_id (FK намеренно отсутствует): %v", err)
	}

	// 6) Каскад: удаление проекта удаляет его группы.
	if _, err := pool.Exec(ctx, "DELETE FROM projects WHERE id=$1", projectID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_groups WHERE project_id=$1", projectID).Scan(&left); err != nil {
		t.Fatalf("count incident_groups: %v", err)
	}
	if left != 0 {
		t.Fatalf("incident_groups after project delete = %d, want 0", left)
	}

	// 7) Down откатывается.
	if err := db.MigratePGTo(dsn, 78); err != nil {
		t.Fatalf("migrate down to 78: %v", err)
	}
}
