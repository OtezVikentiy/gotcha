package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Контракт перехода slug → public_id: 0062 expand, 0063 contract (удаляет slug, которую код больше
// не читает). down восстанавливает колонку nullable/заполненную, без NOT NULL/UNIQUE (это зона 0062).
func TestMigrate0063StatusPageDropSlugThenRestoreBack(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 62); err != nil {
		t.Fatalf("migrate to 62: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, redirectedPageID, plainPageID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m63', 'M63', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm63', 'M63') RETURNING id", orgID)

	// Так выглядит страница, пережившая апгрейд 0062 (redirects заполняет 0062.up) — здесь эмулируем
	// вручную под явным public_id.
	mustScan(t, pool, &redirectedPageID,
		`INSERT INTO status_pages (project_id, public_id, slug, title, description, enabled)
		 VALUES ($1, 'p_redirected000000000000', 'legacy-x', 'Redirected', '', true)
		 RETURNING id`, projectID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO status_page_redirects (legacy_slug, status_page_id) VALUES ('legacy-x', $1)`,
		redirectedPageID); err != nil {
		t.Fatalf("insert status_page_redirects: %v", err)
	}

	// Так выглядит страница, созданная в новой модели (public_id есть, slug NULL, редиректа нет).
	mustScan(t, pool, &plainPageID,
		`INSERT INTO status_pages (project_id, public_id, title, description, enabled)
		 VALUES ($1, 'p_plain0000000000000000', 'Plain', '', true)
		 RETURNING id`, projectID)

	// Эмулирует старый бинарь на rolling-deploy: создана ПОСЛЕ 0062, backfill её не задел, redirects нет
	// вручную. 0063.up обязан заморозить slug сам — иначе после DROP COLUMN 301-адрес потеряется навсегда.
	var postUpgradePageID int64
	mustScan(t, pool, &postUpgradePageID,
		`INSERT INTO status_pages (project_id, public_id, slug, title, description, enabled)
		 VALUES ($1, 'p_post0062000000000000', 'post-0062', 'PostUpgrade', '', true)
		 RETURNING id`, projectID)

	if err := db.MigratePGTo(dsn, 63); err != nil {
		t.Fatalf("migrate to 63: %v", err)
	}

	var postUpgradeRedirectPageID int64
	if err := pool.QueryRow(ctx,
		"SELECT status_page_id FROM status_page_redirects WHERE legacy_slug = 'post-0062'").
		Scan(&postUpgradeRedirectPageID); err != nil {
		t.Fatalf("select status_page_redirects по post-0062 после 0063 up: %v", err)
	}
	if postUpgradeRedirectPageID != postUpgradePageID {
		t.Fatalf("status_page_redirects.status_page_id = %d, want %d (freeze защитного INSERT в 0063.up не сработал)",
			postUpgradeRedirectPageID, postUpgradePageID)
	}

	var slugColExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		     WHERE table_name = 'status_pages' AND column_name = 'slug'
		 )`).Scan(&slugColExists); err != nil {
		t.Fatalf("check slug column: %v", err)
	}
	if slugColExists {
		t.Fatal("колонка slug должна исчезнуть после 0063 up")
	}

	var publicIDColExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		     WHERE table_name = 'status_pages' AND column_name = 'public_id'
		 )`).Scan(&publicIDColExists); err != nil {
		t.Fatalf("check public_id column: %v", err)
	}
	if !publicIDColExists {
		t.Fatal("колонка public_id не должна пострадать от 0063 up")
	}

	// Публичный адрес по-прежнему работает — public_id никак не задет.
	var title string
	if err := pool.QueryRow(ctx,
		"SELECT title FROM status_pages WHERE public_id = 'p_redirected000000000000'").Scan(&title); err != nil {
		t.Fatalf("select по public_id после 0063 up: %v", err)
	}
	if title != "Redirected" {
		t.Fatalf("title = %q, want %q", title, "Redirected")
	}

	if err := db.MigratePGTo(dsn, 62); err != nil {
		t.Fatalf("migrate down to 62: %v", err)
	}

	var slugColExistsAfterDown bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.columns
		     WHERE table_name = 'status_pages' AND column_name = 'slug'
		 )`).Scan(&slugColExistsAfterDown); err != nil {
		t.Fatalf("check slug column после отката: %v", err)
	}
	if !slugColExistsAfterDown {
		t.Fatal("колонка slug должна вернуться после отката 0063")
	}

	var slugNullable string
	if err := pool.QueryRow(ctx,
		`SELECT is_nullable FROM information_schema.columns
		 WHERE table_name = 'status_pages' AND column_name = 'slug'`).Scan(&slugNullable); err != nil {
		t.Fatalf("check slug is_nullable: %v", err)
	}
	if slugNullable != "YES" {
		t.Fatal("slug должен остаться nullable после отката 0063 (NOT NULL — зона 0062.down, не 0063.down)")
	}

	var slugUniqueConstraintExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.table_constraints
		     WHERE table_name = 'status_pages' AND constraint_type = 'UNIQUE'
		       AND constraint_name = 'status_pages_slug_key'
		 )`).Scan(&slugUniqueConstraintExists); err != nil {
		t.Fatalf("check status_pages_slug_key: %v", err)
	}
	if slugUniqueConstraintExists {
		t.Fatal("UNIQUE на slug не должен вернуться после отката 0063 (это зона 0062.down)")
	}

	var redirectedSlugBack string
	if err := pool.QueryRow(ctx,
		"SELECT slug FROM status_pages WHERE id = $1", redirectedPageID).Scan(&redirectedSlugBack); err != nil {
		t.Fatalf("select slug строки с редиректом после отката: %v", err)
	}
	if redirectedSlugBack != "legacy-x" {
		t.Fatalf("slug = %q после отката, want легаси-адрес из status_page_redirects 'legacy-x'", redirectedSlugBack)
	}

	var plainSlugBack string
	if err := pool.QueryRow(ctx,
		"SELECT slug FROM status_pages WHERE id = $1", plainPageID).Scan(&plainSlugBack); err != nil {
		t.Fatalf("select slug строки без редиректа после отката: %v", err)
	}
	if plainSlugBack != "p_plain0000000000000000" {
		t.Fatalf("slug = %q после отката, want fallback на public_id 'p_plain0000000000000000'", plainSlugBack)
	}
}
