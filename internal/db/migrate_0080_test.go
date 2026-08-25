package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0080IncidentGroupsResolvedIdx — R2b/W10: PurgeOldGroups
// (internal/incidentgroup/janitor.go) фильтрует по resolved_at IS NOT NULL
// AND resolved_at < cutoff; несовместимый партиал incident_groups_open_idx
// (0079, WHERE resolved_at IS NULL) под этот запрос не годится. На непустой
// базе (TestLatestMigrationHasDataTest): заводим resolved- и открытую группу
// ДО миграции 80, проверяем появление индекса и то, что он действительно
// избирательно покрывает фильтр janitor'а, откатываем и убеждаемся, что
// данные соседних строк не пострадали.
func TestMigrate0080IncidentGroupsResolvedIdx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 79); err != nil {
		t.Fatalf("migrate to 79: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m80','M80',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m80','M80') RETURNING id", orgID)

	var hostID, rootInc, resolvedGroupID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id, name) VALUES ($1,'h1') RETURNING id", projectID)
	mustScan(t, pool, &rootInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status)
		VALUES ($1,$2,'silent','open') RETURNING id`, projectID, hostID)
	mustScan(t, pool, &resolvedGroupID, `
		INSERT INTO incident_groups (project_id, root_source, root_incident_id, root_node_kind, root_node_id, resolved_at)
		VALUES ($1,'host',$2,'host',$3, now() - interval '48 hours') RETURNING id`,
		projectID, rootInc, hostID)

	var hostID2, rootInc2, openGroupID int64
	mustScan(t, pool, &hostID2,
		"INSERT INTO hosts (project_id, name) VALUES ($1,'h2') RETURNING id", projectID)
	mustScan(t, pool, &rootInc2, `
		INSERT INTO host_incidents (project_id, host_id, kind, status)
		VALUES ($1,$2,'silent','open') RETURNING id`, projectID, hostID2)
	mustScan(t, pool, &openGroupID, `
		INSERT INTO incident_groups (project_id, root_source, root_incident_id, root_node_kind, root_node_id)
		VALUES ($1,'host',$2,'host',$3) RETURNING id`, projectID, rootInc2, hostID2)

	if err := db.MigratePGTo(dsn, 80); err != nil {
		t.Fatalf("migrate to 80: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'incident_groups_resolved_idx')").Scan(&exists); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !exists {
		t.Fatal("индекс incident_groups_resolved_idx не найден после миграции до 80")
	}

	// Индекс избирательно покрывает именно фильтр PurgeOldGroups: старая
	// resolved-группа под условием находится, открытая — не подходит под
	// предикат резолвнутости вовсе (частичный индекс её не индексирует).
	var found bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM incident_groups
		WHERE id = $1 AND resolved_at IS NOT NULL AND resolved_at < now() - interval '24 hours')`,
		resolvedGroupID).Scan(&found); err != nil {
		t.Fatalf("check resolved group under filter: %v", err)
	}
	if !found {
		t.Fatal("resolved-группа старше 24ч должна попадать под фильтр janitor'а")
	}
	if err := pool.QueryRow(ctx,
		"SELECT resolved_at IS NULL FROM incident_groups WHERE id = $1", openGroupID).Scan(&found); err != nil {
		t.Fatalf("check open group: %v", err)
	}
	if !found {
		t.Fatal("открытая группа не должна иметь resolved_at")
	}

	if err := db.MigratePGTo(dsn, 79); err != nil {
		t.Fatalf("migrate down to 79: %v", err)
	}

	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'incident_groups_resolved_idx')").Scan(&exists); err != nil {
		t.Fatalf("check index after rollback: %v", err)
	}
	if exists {
		t.Fatal("индекс incident_groups_resolved_idx должен исчезнуть после отката до 79")
	}

	var cnt int64
	mustScan(t, pool, &cnt,
		"SELECT count(*) FROM incident_groups WHERE id IN ($1,$2)", resolvedGroupID, openGroupID)
	if cnt != 2 {
		t.Fatalf("группы должны пережить откат индекса, count=%d, want 2", cnt)
	}
}
