package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0082ExportJobsCreatedByIdx — TestForeignKeysHaveCoveringIndex
// (internal/guards/fkindex_test.go): export_jobs_created_by_fkey (0081) без
// покрывающего индекса. На непустой базе (TestLatestMigrationHasDataTest):
// заводим заявку ДО миграции 82, проверяем появление индекса и то, что он
// действительно покрывает поиск по created_by (тот же профиль запроса, что
// каскадное удаление пользователя строит по FK), откатываем и убеждаемся,
// что строка export_jobs пережила откат индекса.
func TestMigrate0082ExportJobsCreatedByIdx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 81); err != nil {
		t.Fatalf("migrate to 81: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, userID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m82','M82',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m82','M82') RETURNING id", orgID)
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m82@example.com', 'x') RETURNING id")

	var jobID int64
	mustScan(t, pool, &jobID, `
		INSERT INTO export_jobs (project_id, created_by, kind, format)
		VALUES ($1,$2,'issues','csv') RETURNING id`, projectID, userID)

	if err := db.MigratePGTo(dsn, 82); err != nil {
		t.Fatalf("migrate to 82: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'export_jobs_created_by_idx')").Scan(&exists); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !exists {
		t.Fatal("индекс export_jobs_created_by_idx не найден после миграции до 82")
	}

	var found bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM export_jobs WHERE id = $1 AND created_by = $2)",
		jobID, userID).Scan(&found); err != nil {
		t.Fatalf("check job by created_by: %v", err)
	}
	if !found {
		t.Fatal("заявка не найдена по created_by после миграции до 82")
	}

	if err := db.MigratePGTo(dsn, 81); err != nil {
		t.Fatalf("migrate down to 81: %v", err)
	}

	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'export_jobs_created_by_idx')").Scan(&exists); err != nil {
		t.Fatalf("check index after rollback: %v", err)
	}
	if exists {
		t.Fatal("индекс export_jobs_created_by_idx должен исчезнуть после отката до 81")
	}

	var cnt int64
	mustScan(t, pool, &cnt, "SELECT count(*) FROM export_jobs WHERE id = $1", jobID)
	if cnt != 1 {
		t.Fatalf("заявка должна пережить откат индекса, count=%d, want 1", cnt)
	}
}
