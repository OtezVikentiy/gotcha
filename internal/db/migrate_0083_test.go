package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// nullable БЕЗ DEFAULT безопасно на непустой таблице — проверяем явно: строка до миграции остаётся
// с NULL, а экран выгрузок трактует это как «подсказки о причине нет» (Job.FailureReasonKey).
func TestMigrate0083AddsNullableFailureReasonKey(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 82); err != nil {
		t.Fatalf("migrate to 82: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, userID, jobID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name) VALUES ('m83', 'M83') RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm83', 'M83') RETURNING id", orgID)
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m83@example.com', 'x') RETURNING id")
	// Заявка, завершившаяся отказом до миграции — ровно то, что увидит любая инсталляция при апгрейде.
	mustScan(t, pool, &jobID, `
		INSERT INTO export_jobs (project_id, created_by, kind, format, status)
		VALUES ($1, $2, 'issues', 'csv', 'failed') RETURNING id`, projectID, userID)

	if err := db.MigratePGTo(dsn, 83); err != nil {
		t.Fatalf("migrate to 83: %v", err)
	}

	var reasonKey *string
	if err := pool.QueryRow(ctx,
		"SELECT failure_reason_key FROM export_jobs WHERE id = $1", jobID).Scan(&reasonKey); err != nil {
		t.Fatalf("select failure_reason_key: %v", err)
	}
	if reasonKey != nil {
		t.Fatalf("failure_reason_key = %q для строки, заведённой до миграции, want NULL", *reasonKey)
	}

	// Наличие и тип колонки проверяются этим же UPDATE — ошибка провалила бы Exec.
	mustExec(t, pool,
		"UPDATE export_jobs SET failure_reason_key = 'exports.mail.failed.reason.disk_full' WHERE id = $1", jobID)
	if err := pool.QueryRow(ctx,
		"SELECT failure_reason_key FROM export_jobs WHERE id = $1", jobID).Scan(&reasonKey); err != nil {
		t.Fatalf("select failure_reason_key после UPDATE: %v", err)
	}
	if reasonKey == nil || *reasonKey != "exports.mail.failed.reason.disk_full" {
		t.Fatalf("failure_reason_key после UPDATE = %v, want exports.mail.failed.reason.disk_full", reasonKey)
	}
}
