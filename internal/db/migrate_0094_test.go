package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// idempotency_key — nullable без backfill, старые строки вне частичного
// индекса и обязаны переживать миграцию независимо от дублей среди них.
func TestMigrate0094PreservesExistingOutboxRowsIncludingDuplicates(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 93); err != nil {
		t.Fatalf("migrate to 93: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, channelID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m94', 'M94', 1000000) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm94', 'M94') RETURNING id", orgID)
	mustScan(t, pool, &channelID,
		"INSERT INTO alert_channels (project_id, kind, target) VALUES ($1, 'webhook', 'https://example.com/hook') RETURNING id",
		projectID)

	// Намеренный дубль по (канал, payload) — у старых строк idempotency_key нет.
	for i := 0; i < 3; i++ {
		mustExec(t, pool,
			"INSERT INTO notification_outbox (channel_id, payload) VALUES ($1, $2::jsonb)",
			channelID, `{"kind":"down"}`)
	}

	var before int64
	mustScan(t, pool, &before, "SELECT count(*) FROM notification_outbox")
	if before != 3 {
		t.Fatalf("test setup: before = %d, want 3", before)
	}

	if err := db.MigratePGTo(dsn, 94); err != nil {
		t.Fatalf("migrate to 94: %v", err)
	}

	var after int64
	mustScan(t, pool, &after, "SELECT count(*) FROM notification_outbox")
	if after != before {
		t.Fatalf("после миграции строк = %d, want %d (пред-существующие дубли не должны были быть тронуты)", after, before)
	}

	var nullKeys int64
	mustScan(t, pool, &nullKeys, "SELECT count(*) FROM notification_outbox WHERE idempotency_key IS NULL")
	if nullKeys != before {
		t.Fatalf("idempotency_key IS NULL у %d строк, want %d (миграция не должна была backfill'ить старые строки)", nullKeys, before)
	}

	// Индекс реально работает на новых строках: конфликт по ключу отвергается.
	mustExec(t, pool,
		"INSERT INTO notification_outbox (channel_id, payload, idempotency_key) VALUES ($1, $2::jsonb, 'uptime:open:1:1')",
		channelID, `{"kind":"down"}`)
	if _, err := pool.Exec(ctx,
		"INSERT INTO notification_outbox (channel_id, payload, idempotency_key) VALUES ($1, $2::jsonb, 'uptime:open:1:1')",
		channelID, `{"kind":"down"}`); err == nil {
		t.Fatalf("второй INSERT с тем же idempotency_key прошёл, want нарушение уникального индекса")
	}
}
