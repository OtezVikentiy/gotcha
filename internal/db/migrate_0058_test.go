package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки. На момент этой правки новейшая —
// 0058_perf_issue_description.up.sql.

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0058BackfillsDescriptionFromTitles — backfill №132 извлекает
// параметр из накопленных русских заголовков по двум известным префиксам,
// не трогая ни сами title, ни строки, под префиксы не подошедшие (для них
// работает fallback чтения title на рендере).
func TestMigrate0058BackfillsDescriptionFromTitles(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 57); err != nil {
		t.Fatalf("migrate to 57: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name) VALUES ('m58', 'M58') RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name, platform) VALUES ("+
			"(SELECT id FROM organizations WHERE slug='m58'), 'm58p', 'M58P', 'go') RETURNING id")

	seed := []struct{ fp, kind, title string }{
		{"fp1", "n_plus_one", "N+1 запросов: SELECT * FROM users WHERE id = ?"},
		{"fp2", "slow_db_query", "Медленный запрос: SELECT count(*) FROM events"},
		{"fp3", "http_flood", "Лавина HTTP-вызовов: GET /orders"},
		{"fp4", "n_plus_one", "что-то стороннее без известного префикса"},
	}
	for _, s := range seed {
		if _, err := pool.Exec(ctx,
			"INSERT INTO perf_issues (project_id, fingerprint, kind, title) VALUES ($1, $2, $3, $4)",
			projectID, s.fp, s.kind, s.title); err != nil {
			t.Fatalf("seed %s: %v", s.fp, err)
		}
	}

	if err := db.MigratePGTo(dsn, 58); err != nil {
		t.Fatalf("migrate to 58: %v", err)
	}

	want := map[string]string{
		"fp1": "SELECT * FROM users WHERE id = ?",
		"fp2": "SELECT count(*) FROM events",
		"fp3": "", // у http_flood параметр — culprit, description не извлекается
		"fp4": "", // незнакомый заголовок остаётся под fallback чтения title
	}
	for _, s := range seed {
		var title, description string
		if err := pool.QueryRow(ctx,
			"SELECT title, description FROM perf_issues WHERE project_id = $1 AND fingerprint = $2",
			projectID, s.fp).Scan(&title, &description); err != nil {
			t.Fatalf("select %s: %v", s.fp, err)
		}
		if title != s.title {
			t.Errorf("%s: title изменён миграцией: %q -> %q", s.fp, s.title, title)
		}
		if description != want[s.fp] {
			t.Errorf("%s: description = %q, want %q", s.fp, description, want[s.fp])
		}
	}

	// Дефолт title работает: INSERT без title (так пишет детектор после №132)
	// не падает и оставляет пустую строку.
	if _, err := pool.Exec(ctx,
		"INSERT INTO perf_issues (project_id, fingerprint, kind, description) VALUES ($1, 'fp5', 'n_plus_one', 'SELECT 1')",
		projectID); err != nil {
		t.Fatalf("insert без title после 0058: %v", err)
	}
}
