package db_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Переход публичного адреса slug → public_id: 0062 только expand (slug ослабляется, но остаётся,
// contract — 0063). Проверяем public_id, freeze в status_page_redirects, снятие NOT NULL/UNIQUE, и up/down/up.
func TestMigrate0062PublicIDExpandThenContractBack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 61); err != nil {
		t.Fatalf("migrate to 61: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, pageID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m62', 'M62', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm62', 'M62') RETURNING id", orgID)
	mustScan(t, pool, &pageID,
		`INSERT INTO status_pages (project_id, slug, title, description, enabled)
		 VALUES ($1, 'acme', 'Acme', '', true)
		 RETURNING id`, projectID)

	if err := db.MigratePGTo(dsn, 62); err != nil {
		t.Fatalf("migrate to 62: %v", err)
	}

	var publicID string
	if err := pool.QueryRow(ctx,
		"SELECT public_id FROM status_pages WHERE id = $1", pageID).Scan(&publicID); err != nil {
		t.Fatalf("select public_id после миграции: %v", err)
	}
	if !strings.HasPrefix(publicID, "p_") || len(publicID) != len("p_")+24 {
		t.Fatalf("public_id = %q, want префикс p_ и 24 hex-символа (12 байт)", publicID)
	}

	var redirectPageID int64
	if err := pool.QueryRow(ctx,
		"SELECT status_page_id FROM status_page_redirects WHERE legacy_slug = 'acme'").Scan(&redirectPageID); err != nil {
		t.Fatalf("select status_page_redirects после миграции: %v", err)
	}
	if redirectPageID != pageID {
		t.Fatalf("status_page_redirects.status_page_id = %d, want %d", redirectPageID, pageID)
	}

	// slug теперь nullable и не глобально-уникален — новая страница без slug проходит.
	var newPageID int64
	mustScan(t, pool, &newPageID,
		`INSERT INTO status_pages (project_id, public_id, title, description, enabled)
		 VALUES ($1, 'p_test', 'New', '', true)
		 RETURNING id`, projectID)

	// Регрессия: INSERT в форме старого бинаря (пять колонок, без public_id) падал NOT NULL до DEFAULT —
	// между выкаткой схемы и кода бинарь не мог бы создать ни одной статус-страницы.
	var oldBinaryPageID int64
	mustScan(t, pool, &oldBinaryPageID,
		`INSERT INTO status_pages (project_id, slug, title, description, enabled)
		 VALUES ($1, 'old-binary', 'Old', '', true)
		 RETURNING id`, projectID)
	var oldBinaryPublicID string
	if err := pool.QueryRow(ctx,
		"SELECT public_id FROM status_pages WHERE id = $1", oldBinaryPageID).Scan(&oldBinaryPublicID); err != nil {
		t.Fatalf("select public_id страницы, вставленной как старый бинарь: %v", err)
	}
	if !strings.HasPrefix(oldBinaryPublicID, "p_") || len(oldBinaryPublicID) != len("p_")+24 {
		t.Fatalf("public_id = %q для INSERT без public_id, want DEFAULT формата p_<24 hex>", oldBinaryPublicID)
	}

	if err := db.MigratePGTo(dsn, 61); err != nil {
		t.Fatalf("migrate down to 61: %v", err)
	}

	var slugBack string
	if err := pool.QueryRow(ctx,
		"SELECT slug FROM status_pages WHERE id = $1", pageID).Scan(&slugBack); err != nil {
		t.Fatalf("select slug после отката: %v", err)
	}
	if slugBack != "acme" {
		t.Fatalf("slug = %q после отката, want 'acme'", slugBack)
	}

	// down безусловно затирал непустой slug на public_id, теряя реальный slug у строк старого бинаря
	// (есть slug, нет записи в status_page_redirects, public_id из DEFAULT). Проверяем обе стороны.
	var oldBinarySlugBack string
	if err := pool.QueryRow(ctx,
		"SELECT slug FROM status_pages WHERE id = $1", oldBinaryPageID).Scan(&oldBinarySlugBack); err != nil {
		t.Fatalf("select slug строки старого бинаря после отката: %v", err)
	}
	if oldBinarySlugBack != "old-binary" {
		t.Fatalf("slug = %q после отката, want настоящий 'old-binary' (не public_id %q) — down затёр реальный slug",
			oldBinarySlugBack, oldBinaryPublicID)
	}

	// Строка без исходного slug и без редиректа — единственный верный fallback это её public_id.
	var newSlugBack string
	if err := pool.QueryRow(ctx,
		"SELECT slug FROM status_pages WHERE id = $1", newPageID).Scan(&newSlugBack); err != nil {
		t.Fatalf("select slug строки без исходного slug после отката: %v", err)
	}
	if newSlugBack != "p_test" {
		t.Fatalf("slug = %q после отката, want fallback на public_id 'p_test'", newSlugBack)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO status_pages (project_id, slug, title, description, enabled)
		 VALUES ($1, 'acme', 'Dup', '', true)`, projectID); err == nil {
		t.Fatal("insert с дублирующимся slug должен упасть unique-violation после отката")
	}

	var publicIDColExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		     WHERE table_name = 'status_pages' AND column_name = 'public_id'
		 )`).Scan(&publicIDColExists); err != nil {
		t.Fatalf("check public_id column: %v", err)
	}
	if publicIDColExists {
		t.Fatal("колонка public_id должна исчезнуть после отката 0062")
	}

	var redirectsTableExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.tables
		     WHERE table_name = 'status_page_redirects'
		 )`).Scan(&redirectsTableExists); err != nil {
		t.Fatalf("check status_page_redirects table: %v", err)
	}
	if redirectsTableExists {
		t.Fatal("таблица status_page_redirects должна исчезнуть после отката 0062")
	}
}
