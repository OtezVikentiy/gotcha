package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Тот же приём, что 0018, отдельной колонкой — падение под собственным номером,
// не молчаливое покрытие соседней миграцией.
func TestMigrate0020EventQuotaDefaultResetsLegacyValueOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 19); err != nil {
		t.Fatalf("migrate to 19: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var legacyID, customID int64
	mustScan(t, pool, &legacyID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m20-legacy', 'M20 Legacy', 1000000) RETURNING id")
	mustScan(t, pool, &customID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m20-custom', 'M20 Custom', 400000) RETURNING id")

	if err := db.MigratePGTo(dsn, 20); err != nil {
		t.Fatalf("migrate to 20: %v", err)
	}
	assertEventQuota(t, pool, legacyID, 0)
	assertEventQuota(t, pool, customID, 400000)

	var newOrgID int64
	mustScan(t, pool, &newOrgID,
		"INSERT INTO organizations (slug, name) VALUES ('m20-new', 'M20 New') RETURNING id")
	assertEventQuota(t, pool, newOrgID, 0)

	if err := db.MigratePGTo(dsn, 19); err != nil {
		t.Fatalf("migrate down to 19: %v", err)
	}
	assertEventQuota(t, pool, legacyID, 0)
	assertEventQuota(t, pool, customID, 400000)

	var revertedDefaultID int64
	mustScan(t, pool, &revertedDefaultID,
		"INSERT INTO organizations (slug, name) VALUES ('m20-reverted', 'M20 Reverted') RETURNING id")
	assertEventQuota(t, pool, revertedDefaultID, 1000000)
}

func assertEventQuota(t *testing.T, pool *pgxpool.Pool, orgID, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(context.Background(),
		"SELECT event_quota FROM organizations WHERE id = $1", orgID).Scan(&got); err != nil {
		t.Fatalf("select event_quota org=%d: %v", orgID, err)
	}
	if got != want {
		t.Errorf("org=%d event_quota = %d, want %d", orgID, got, want)
	}
}
