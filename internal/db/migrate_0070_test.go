package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// checkAndCount списывает разность (счётчик − предобраз) — ненулевой мусор в logs_count_before
// дал бы неверное списание; log_quota NOT NULL DEFAULT 0 — nullable сломала бы Scan в org.Get/OrgsOf.
func TestMigrate0070AddsLogQuotaAndUsageColumns(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 69); err != nil {
		t.Fatalf("migrate to 69: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m70', 'M70', 1000) RETURNING id")
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_usage (org_id, period_month, events_count)
		 VALUES ($1, date_trunc('month', now())::date, 42)`, orgID); err != nil {
		t.Fatalf("seed org_usage: %v", err)
	}

	if err := db.MigratePGTo(dsn, 70); err != nil {
		t.Fatalf("migrate to 70: %v", err)
	}

	var events, logsCount, logsCountBefore, droppedLogs int64
	if err := pool.QueryRow(ctx,
		`SELECT events_count, logs_count, logs_count_before, dropped_logs
		 FROM org_usage WHERE org_id = $1`, orgID,
	).Scan(&events, &logsCount, &logsCountBefore, &droppedLogs); err != nil {
		t.Fatalf("select org_usage после миграции: %v", err)
	}
	if events != 42 {
		t.Fatalf("events_count после 0070 = %d, want 42: потребление изменено миграцией", events)
	}
	if logsCount != 0 || logsCountBefore != 0 || droppedLogs != 0 {
		t.Fatalf("новые колонки логов = (%d, %d, %d), want (0, 0, 0) для строки, заведённой до миграции",
			logsCount, logsCountBefore, droppedLogs)
	}

	var logQuota int64
	if err := pool.QueryRow(ctx,
		"SELECT log_quota FROM organizations WHERE id = $1", orgID).Scan(&logQuota); err != nil {
		t.Fatalf("select log_quota: %v", err)
	}
	if logQuota != 0 {
		t.Fatalf("log_quota = %d, want 0 (безлимит по умолчанию для орги, созданной до миграции)", logQuota)
	}

	if err := db.MigratePGTo(dsn, 69); err != nil {
		t.Fatalf("migrate down to 69: %v", err)
	}

	for _, col := range []string{"logs_count", "logs_count_before", "dropped_logs"} {
		var columnExists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'org_usage' AND column_name = $1
			)`, col).Scan(&columnExists); err != nil {
			t.Fatalf("check %s column: %v", col, err)
		}
		if columnExists {
			t.Fatalf("колонка org_usage.%s должна исчезнуть после отката 0070", col)
		}
	}
	var columnExists bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'organizations' AND column_name = 'log_quota'
		)`).Scan(&columnExists); err != nil {
		t.Fatalf("check log_quota column: %v", err)
	}
	if columnExists {
		t.Fatal("колонка organizations.log_quota должна исчезнуть после отката 0070")
	}

	// Строки organizations/org_usage откат не трогает — DROP COLUMN, не DROP TABLE.
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM organizations WHERE id = $1", orgID).Scan(&name); err != nil {
		t.Fatalf("org row must survive down-migration: %v", err)
	}
	if name != "M70" {
		t.Fatalf("name = %q, want M70", name)
	}
}
