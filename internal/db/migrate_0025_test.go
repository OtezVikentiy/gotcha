package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Строка очереди обязана пережить вычистку secret — доставка резолвит его по channel_id, а не
// из payload; поломка здесь означает потерянные уведомления, а не просто утечку.
func TestMigrate0025OutboxDropSecretKeepsRowDeliverable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 24); err != nil {
		t.Fatalf("migrate to 24: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, channelID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m25', 'M25', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm25', 'M25') RETURNING id", orgID)
	mustScan(t, pool, &channelID,
		`INSERT INTO alert_channels (project_id, kind, enabled, target, secret)
		 VALUES ($1, 'webhook', true, 'https://example.com/hook', 'hmac-secret')
		 RETURNING id`, projectID)

	var withSecretID, withoutSecretID int64
	mustScan(t, pool, &withSecretID,
		`INSERT INTO notification_outbox (channel_id, payload, status)
		 VALUES ($1, $2::jsonb, 'pending')
		 RETURNING id`, channelID,
		`{"secret":"hmac-secret","channel_kind":"webhook","target":"https://example.com/hook","message":"issue opened"}`)
	mustScan(t, pool, &withoutSecretID,
		`INSERT INTO notification_outbox (channel_id, payload, status)
		 VALUES ($1, $2::jsonb, 'pending')
		 RETURNING id`, channelID,
		`{"channel_kind":"webhook","target":"https://example.com/hook","message":"already clean"}`)

	if err := db.MigratePGTo(dsn, 25); err != nil {
		t.Fatalf("migrate to 25: %v", err)
	}

	assertOutboxPayload(t, pool, withSecretID, "pending",
		map[string]string{"channel_kind": "webhook", "target": "https://example.com/hook", "message": "issue opened"})
	assertOutboxPayload(t, pool, withoutSecretID, "pending",
		map[string]string{"channel_kind": "webhook", "target": "https://example.com/hook", "message": "already clean"})

	if err := db.MigratePGTo(dsn, 24); err != nil {
		t.Fatalf("migrate down to 24: %v", err)
	}
	assertOutboxPayload(t, pool, withSecretID, "pending",
		map[string]string{"channel_kind": "webhook", "target": "https://example.com/hook", "message": "issue opened"})

	if err := db.MigratePGTo(dsn, 25); err != nil {
		t.Fatalf("migrate up to 25 again: %v", err)
	}
	assertOutboxPayload(t, pool, withSecretID, "pending",
		map[string]string{"channel_kind": "webhook", "target": "https://example.com/hook", "message": "issue opened"})
}

func assertOutboxPayload(t *testing.T, pool *pgxpool.Pool, id int64, wantStatus string, wantKeys map[string]string) {
	t.Helper()
	var status string
	var hasSecret bool
	if err := pool.QueryRow(context.Background(),
		"SELECT status, jsonb_exists(payload, 'secret') FROM notification_outbox WHERE id = $1", id).
		Scan(&status, &hasSecret); err != nil {
		t.Fatalf("select outbox id=%d: %v", id, err)
	}
	if status != wantStatus {
		t.Errorf("id=%d status = %q, want %q — строка не должна выпадать из очереди", id, status, wantStatus)
	}
	if hasSecret {
		t.Errorf("id=%d payload всё ещё содержит secret", id)
	}
	for k, want := range wantKeys {
		var got string
		if err := pool.QueryRow(context.Background(),
			"SELECT payload->>$2 FROM notification_outbox WHERE id = $1", id, k).Scan(&got); err != nil {
			t.Fatalf("select payload[%s] id=%d: %v", k, id, err)
		}
		if got != want {
			t.Errorf("id=%d payload[%s] = %q, want %q", id, k, got, want)
		}
	}
}
