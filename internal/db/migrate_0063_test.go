package db_test

// TestLatestMigrationHasDataTest (internal/guards) требует, чтобы НОВЕЙШАЯ
// миграция PostgreSQL приезжала с тестом на непустой базе — db.MigratePGTo на
// схему, уже содержащую строки. На момент этой правки новейшая —
// 0063_status_page_drop_slug.up.sql.

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0063StatusPageDropSlugThenRestoreBack — T5, контракт перехода
// публичного адреса статус-страницы со slug на public_id (0062 — expand,
// 0063 — contract: удаляет колонку, которую код больше не читает и не
// пишет, T2-T4). Заводим ДВЕ строки на схеме после 0062: одну с записью в
// status_page_redirects (легаси-адрес заморожен там миграцией 0062), другую
// без неё (страница уже новой модели). Проверяем: (а) up удаляет колонку
// slug целиком — SELECT по ней должен быть невозможен, sql-запрос через
// information_schema; (б) down возвращает колонку, nullable, заполненную —
// из redirects для первой строки, из public_id для второй (см. брифа §2 —
// down.sql воспроизводит только 0062-состояние, НЕ ставит NOT NULL/UNIQUE).
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

	// Строка с замороженным легаси-адресом: slug + public_id + запись в
	// status_page_redirects — так выглядит страница, пережившая апгрейд 0062
	// (слуг был у неё до миграции, 0062.up сама заполняет redirects, здесь
	// эмулируем это вручную под явным public_id).
	mustScan(t, pool, &redirectedPageID,
		`INSERT INTO status_pages (project_id, public_id, slug, title, description, enabled)
		 VALUES ($1, 'p_redirected000000000000', 'legacy-x', 'Redirected', '', true)
		 RETURNING id`, projectID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO status_page_redirects (legacy_slug, status_page_id) VALUES ('legacy-x', $1)`,
		redirectedPageID); err != nil {
		t.Fatalf("insert status_page_redirects: %v", err)
	}

	// Строка без легаси-адреса: создана уже в новой модели (public_id есть,
	// slug NULL, редиректа нет) — так выглядит страница, созданная кодом
	// после T2/T3.
	mustScan(t, pool, &plainPageID,
		`INSERT INTO status_pages (project_id, public_id, title, description, enabled)
		 VALUES ($1, 'p_plain0000000000000000', 'Plain', '', true)
		 RETURNING id`, projectID)

	// Строка, эмулирующая старый бинарь на переходном окне rolling-deploy:
	// создана ПОСЛЕ 0062 (значит, её slug никогда не проходил через backfill
	// 0062.up — тот отработал один раз, до этой вставки) и без ручной записи
	// в status_page_redirects. 0063.up обязан заморозить такой slug сам,
	// иначе после DROP COLUMN её публичный 301-адрес потеряется навсегда.
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
