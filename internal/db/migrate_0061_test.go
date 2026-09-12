package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Разово нейтрализует уже накопленный last_error (мог нести адрес получателя/URL из RCPT/webhook-
// ответа) — WHERE last_error <> ” бережёт от UPDATE на каждой строке очереди без нужды.
func TestMigrate0061ScrubsLastErrorLeavesEmptyAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 60); err != nil {
		t.Fatalf("migrate to 60: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, channelID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m61', 'M61', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm61', 'M61') RETURNING id", orgID)
	mustScan(t, pool, &channelID,
		`INSERT INTO alert_channels (project_id, kind, enabled, target, secret)
		 VALUES ($1, 'email', true, 'victim@m61.example', '')
		 RETURNING id`, projectID)

	var leakyID, emptyID int64
	mustScan(t, pool, &leakyID,
		`INSERT INTO notification_outbox (channel_id, payload, status, last_error)
		 VALUES ($1, '{}'::jsonb, 'failed', 'notify: smtp rcpt: 550 5.1.1 <victim@m61.example>: Recipient address rejected')
		 RETURNING id`, channelID)
	mustScan(t, pool, &emptyID,
		`INSERT INTO notification_outbox (channel_id, payload, status)
		 VALUES ($1, '{}'::jsonb, 'sent')
		 RETURNING id`, channelID)

	if err := db.MigratePGTo(dsn, 61); err != nil {
		t.Fatalf("migrate to 61: %v", err)
	}

	var leakyErr string
	if err := pool.QueryRow(ctx,
		"SELECT last_error FROM notification_outbox WHERE id = $1", leakyID).Scan(&leakyErr); err != nil {
		t.Fatalf("select leaky row после миграции: %v", err)
	}
	if leakyErr != "[redacted on upgrade]" {
		t.Fatalf("last_error = %q после миграции, want маркер [redacted on upgrade]", leakyErr)
	}

	var emptyErr string
	if err := pool.QueryRow(ctx,
		"SELECT last_error FROM notification_outbox WHERE id = $1", emptyID).Scan(&emptyErr); err != nil {
		t.Fatalf("select empty row после миграции: %v", err)
	}
	if emptyErr != "" {
		t.Fatalf("last_error = %q после миграции, want '' (строка без ошибки не должна была тронута)", emptyErr)
	}
}
