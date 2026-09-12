package db_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Отметка «доверенный получатель» приезжает СНЯТОЙ — иначе канал стал бы доверенным при апгрейде
// и начал возить детали события (потенциально ПДн) получателю без ведома оператора.
func TestMigrate0060DefaultsChannelTrustedFalse(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 59); err != nil {
		t.Fatalf("migrate to 59: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, channelID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m60', 'M60', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm60', 'M60') RETURNING id", orgID)
	mustScan(t, pool, &channelID,
		`INSERT INTO alert_channels (project_id, kind, enabled, target, secret)
		 VALUES ($1, 'telegram', true, '418885689', 'bot-token-m60')
		 RETURNING id`, projectID)

	if err := db.MigratePGTo(dsn, 60); err != nil {
		t.Fatalf("migrate to 60: %v", err)
	}

	var trusted bool
	var target string
	if err := pool.QueryRow(ctx,
		"SELECT target, trusted FROM alert_channels WHERE id = $1", channelID).Scan(&target, &trusted); err != nil {
		t.Fatalf("select channel после миграции: %v", err)
	}
	if target != "418885689" {
		t.Fatalf("target = %q: строка задета миграцией", target)
	}
	if trusted {
		t.Fatal("trusted = true после миграции: обновление сделало канал доверенным без ведома оператора")
	}
}
