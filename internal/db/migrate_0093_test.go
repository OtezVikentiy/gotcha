package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0093LogFiltersFKIndexes — правка находки финального ревью C13
// (internal/guards, TestForeignKeysHaveCoveringIndex): пять внешних ключей
// log_saved_filters/log_default_filters (0092) без покрывающего индекса.
// На непустой базе (TestLatestMigrationHasDataTest, C15): заводим личный и
// общий фильтр ДО миграции 93 (общий — с NULL owner_user_id, ровно тот
// случай, который проверяет частичный индекс), проверяем появление всех
// пяти индексов, откатываем и убеждаемся, что данные не пострадали — тот же
// приём, что и migrate_0089_test.go.
func TestMigrate0093LogFiltersFKIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 92); err != nil {
		t.Fatalf("migrate to 92: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, userID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m93','M93',0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m93','M93') RETURNING id", orgID)
	mustScan(t, pool, &userID,
		"INSERT INTO users (email, password_hash) VALUES ('m93@example.com', 'x') RETURNING id")

	// Личный фильтр: owner_user_id и author_user_id оба заполнены.
	var personalID int64
	mustScan(t, pool, &personalID, `
		INSERT INTO log_saved_filters (project_id, owner_user_id, author_user_id, name)
		VALUES ($1, $2, $2, 'личный') RETURNING id`, projectID, userID)
	// Общий фильтр: owner_user_id NULL (сам смысл "общий") — ровно та строка,
	// которую частичный индекс log_saved_filters_owner_user_id_idx (WHERE
	// owner_user_id IS NOT NULL) обязан НЕ включать, author_user_id при этом
	// заполнен и обязан попасть под свой частичный индекс.
	var sharedID int64
	mustScan(t, pool, &sharedID, `
		INSERT INTO log_saved_filters (project_id, owner_user_id, author_user_id, name)
		VALUES ($1, NULL, $2, 'общий') RETURNING id`, projectID, userID)
	mustExec(t, pool,
		"INSERT INTO log_default_filters (project_id, user_id, filter_id) VALUES ($1,$2,$3)",
		projectID, userID, personalID)

	if err := db.MigratePGTo(dsn, 93); err != nil {
		t.Fatalf("migrate to 93: %v", err)
	}

	for _, name := range []string{
		"log_saved_filters_project_id_idx",
		"log_saved_filters_owner_user_id_idx",
		"log_saved_filters_author_user_id_idx",
		"log_default_filters_user_id_idx",
		"log_default_filters_filter_id_idx",
	} {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)", name).Scan(&exists); err != nil {
			t.Fatalf("check index %s: %v", name, err)
		}
		if !exists {
			t.Errorf("индекс %s не найден после миграции до 93", name)
		}
	}

	// Частичные индексы владельца/автора реально избирательны: общий фильтр
	// (owner_user_id IS NULL) не должен попадать под индекс владельца, но
	// обязан находиться по индексу автора.
	var ownerHit, authorHit bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM log_saved_filters WHERE id = $1 AND owner_user_id IS NOT NULL)",
		sharedID).Scan(&ownerHit); err != nil {
		t.Fatalf("check shared owner predicate: %v", err)
	}
	if ownerHit {
		t.Error("общий фильтр не должен подпадать под owner_user_id IS NOT NULL")
	}
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM log_saved_filters WHERE id = $1 AND author_user_id IS NOT NULL)",
		sharedID).Scan(&authorHit); err != nil {
		t.Fatalf("check shared author predicate: %v", err)
	}
	if !authorHit {
		t.Error("общий фильтр обязан подпадать под author_user_id IS NOT NULL")
	}

	if err := db.MigratePGTo(dsn, 92); err != nil {
		t.Fatalf("migrate down to 92: %v", err)
	}

	for _, name := range []string{
		"log_saved_filters_project_id_idx",
		"log_saved_filters_owner_user_id_idx",
		"log_saved_filters_author_user_id_idx",
		"log_default_filters_user_id_idx",
		"log_default_filters_filter_id_idx",
	} {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)", name).Scan(&exists); err != nil {
			t.Fatalf("check index %s after rollback: %v", name, err)
		}
		if exists {
			t.Errorf("индекс %s должен исчезнуть после отката до 92", name)
		}
	}

	var filterCount, defaultCount int64
	mustScan(t, pool, &filterCount,
		"SELECT count(*) FROM log_saved_filters WHERE id IN ($1,$2)", personalID, sharedID)
	if filterCount != 2 {
		t.Fatalf("log_saved_filters должны пережить откат индексов, count=%d, want 2", filterCount)
	}
	mustScan(t, pool, &defaultCount,
		"SELECT count(*) FROM log_default_filters WHERE project_id = $1 AND user_id = $2", projectID, userID)
	if defaultCount != 1 {
		t.Fatalf("log_default_filters должны пережить откат индексов, count=%d, want 1", defaultCount)
	}
}
