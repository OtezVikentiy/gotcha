package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0089IssuesProjectFirstSeenIdx — issue.Service.CountNewSince
// (задача 7 nav-ia, строка состояния «Обзора») фильтрует WHERE project_id =
// $1 AND first_seen >= $2; issues_project_last_seen_idx (0003) начинается с
// project_id, но продолжается last_seen — не годится под отсечку по
// first_seen. На непустой базе (TestLatestMigrationHasDataTest): заводим два
// issue разного возраста ДО миграции 89, проверяем появление индекса,
// избирательность под фильтр CountNewSince, откатываем и убеждаемся, что
// данные соседних строк не пострадали (тот же приём, что и
// migrate_0080_test.go).
func TestMigrate0089IssuesProjectFirstSeenIdx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 88); err != nil {
		t.Fatalf("migrate to 88: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m89','M89',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m89','M89') RETURNING id", orgID)

	var oldIssueID, newIssueID int64
	mustScan(t, pool, &oldIssueID, `
		INSERT INTO issues (project_id, fingerprint, title, first_seen, last_seen)
		VALUES ($1,'fp-old','old issue', now() - interval '48 hours', now() - interval '48 hours') RETURNING id`,
		projectID)
	mustScan(t, pool, &newIssueID, `
		INSERT INTO issues (project_id, fingerprint, title, first_seen, last_seen)
		VALUES ($1,'fp-new','new issue', now() - interval '1 hour', now() - interval '1 hour') RETURNING id`,
		projectID)

	if err := db.MigratePGTo(dsn, 89); err != nil {
		t.Fatalf("migrate to 89: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'issues_project_first_seen_idx')").Scan(&exists); err != nil {
		t.Fatalf("check index: %v", err)
	}
	if !exists {
		t.Fatal("индекс issues_project_first_seen_idx не найден после миграции до 89")
	}

	// Индекс избирательно покрывает именно фильтр CountNewSince: свежий issue
	// (first_seen час назад) попадает под окно суток, старый (48ч) — нет.
	var found bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM issues
		WHERE id = $1 AND project_id = $2 AND first_seen >= now() - interval '24 hours')`,
		newIssueID, projectID).Scan(&found); err != nil {
		t.Fatalf("check new issue under filter: %v", err)
	}
	if !found {
		t.Fatal("issue с first_seen час назад должен попадать под окно 24ч")
	}
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM issues
		WHERE id = $1 AND project_id = $2 AND first_seen >= now() - interval '24 hours')`,
		oldIssueID, projectID).Scan(&found); err != nil {
		t.Fatalf("check old issue under filter: %v", err)
	}
	if found {
		t.Fatal("issue с first_seen 48ч назад НЕ должен попадать под окно 24ч")
	}

	if err := db.MigratePGTo(dsn, 88); err != nil {
		t.Fatalf("migrate down to 88: %v", err)
	}

	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = 'issues_project_first_seen_idx')").Scan(&exists); err != nil {
		t.Fatalf("check index after rollback: %v", err)
	}
	if exists {
		t.Fatal("индекс issues_project_first_seen_idx должен исчезнуть после отката до 88")
	}

	var cnt int64
	mustScan(t, pool, &cnt,
		"SELECT count(*) FROM issues WHERE id IN ($1,$2)", oldIssueID, newIssueID)
	if cnt != 2 {
		t.Fatalf("issues должны пережить откат индекса, count=%d, want 2", cnt)
	}
}
