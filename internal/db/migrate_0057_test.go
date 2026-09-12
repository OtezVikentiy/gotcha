package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Списание считает разность (счётчик − предобраз) — ненулевой мусор в предобразе у существующей
// строки означал бы неверное «сколько списано» уже в следующем расчёте квоты.
func TestMigrate0057KeepsCountersAndDefaultsPreimage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 56); err != nil {
		t.Fatalf("migrate to 56: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m57', 'M57', 1000) RETURNING id")
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_usage (org_id, period_month, events_count, transactions_count)
		 VALUES ($1, date_trunc('month', now())::date, 128, 7)`, orgID); err != nil {
		t.Fatalf("seed org_usage: %v", err)
	}

	if err := db.MigratePGTo(dsn, 57); err != nil {
		t.Fatalf("migrate to 57: %v", err)
	}

	var events, transactions, eventsBefore, txBefore int64
	if err := pool.QueryRow(ctx,
		`SELECT events_count, transactions_count, events_count_before, transactions_count_before
		 FROM org_usage WHERE org_id = $1`, orgID,
	).Scan(&events, &transactions, &eventsBefore, &txBefore); err != nil {
		t.Fatalf("select org_usage после миграции: %v", err)
	}
	if events != 128 || transactions != 7 {
		t.Fatalf("счётчики после 0057 = (%d, %d), want (128, 7): потребление изменено миграцией", events, transactions)
	}
	if eventsBefore != 0 || txBefore != 0 {
		t.Fatalf("предобраз после 0057 = (%d, %d), want (0, 0): списание считает разность и вернуло бы неверное число",
			eventsBefore, txBefore)
	}
}
