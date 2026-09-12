package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Поиск title/culprit ILIKE не имел индекса поддержки — 0053-0055 ставят pg_trgm и два GIN-индекса.
// Проверяем: строка не пострадала, оба индекса созданы и indisvalid=true (не просто «в каталоге»).
func TestMigrate0055AddsTrgmSearchIndexesWithoutTouchingData(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 52); err != nil {
		t.Fatalf("migrate to 52: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, issueID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m55', 'M55', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm55', 'M55') RETURNING id", orgID)
	mustScan(t, pool, &issueID,
		`INSERT INTO issues (project_id, fingerprint, title, culprit)
		 VALUES ($1, 'fp-m55', 'panic in handler needle55', 'internal/worker/needle55.go:12')
		 RETURNING id`, projectID)

	if err := db.MigratePGTo(dsn, 55); err != nil {
		t.Fatalf("migrate to 55: %v", err)
	}

	var gotTitle, gotCulprit string
	if err := pool.QueryRow(ctx,
		"SELECT title, culprit FROM issues WHERE id = $1", issueID,
	).Scan(&gotTitle, &gotCulprit); err != nil {
		t.Fatalf("select issues после миграции: %v (строка пропала или испорчена)", err)
	}
	if gotTitle != "panic in handler needle55" || gotCulprit != "internal/worker/needle55.go:12" {
		t.Fatalf("issues после 0053-0055 = (title=%q culprit=%q), значения изменились", gotTitle, gotCulprit)
	}

	// Расширение реально установлено (0053), а не просто "миграция не упала".
	var extCount int64
	mustScan(t, pool, &extCount, "SELECT count(*) FROM pg_extension WHERE extname = 'pg_trgm'")
	if extCount != 1 {
		t.Fatalf("pg_extension: pg_trgm найдено %d раз, want 1 — расширение не установлено", extCount)
	}

	// indisvalid=true — не только «объект есть в каталоге» (недостроенный CONCURRENTLY тоже там есть).
	for _, idx := range []string{"issues_title_trgm_idx", "issues_culprit_trgm_idx"} {
		var valid bool
		if err := pool.QueryRow(ctx,
			"SELECT indisvalid FROM pg_index WHERE indexrelid = $1::regclass", idx,
		).Scan(&valid); err != nil {
			t.Fatalf("%s: не найден в pg_index (индекс не создан?): %v", idx, err)
		}
		if !valid {
			t.Fatalf("%s: indisvalid = false — построение CONCURRENTLY сорвалось, индекс недействителен", idx)
		}
	}
}
