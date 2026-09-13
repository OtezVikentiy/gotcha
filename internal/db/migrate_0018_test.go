package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Организация, ровно совпавшая с прежним дефолтом квоты, теряет лимит молча —
// UPDATE бьёт по значению, не по намерению оператора. Down не восстанавливает значения строк.
func TestMigrate0018QuotaDefaultsResetsLegacyValueOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 17); err != nil {
		t.Fatalf("migrate to 17: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var legacyID, customID int64
	mustScan(t, pool, &legacyID,
		"INSERT INTO organizations (slug, name, event_quota, transaction_quota, metric_quota, profile_quota) "+
			"VALUES ('m18-legacy', 'M18 Legacy', 1, 100000, 1000000, 1000000) RETURNING id")
	mustScan(t, pool, &customID,
		"INSERT INTO organizations (slug, name, event_quota, transaction_quota, metric_quota, profile_quota) "+
			"VALUES ('m18-custom', 'M18 Custom', 1, 250000, 500000, 750000) RETURNING id")

	if err := db.MigratePGTo(dsn, 18); err != nil {
		t.Fatalf("migrate to 18: %v", err)
	}

	assertQuotas(t, pool, legacyID, 0, 0, 0)
	assertQuotas(t, pool, customID, 250000, 500000, 750000)

	var newOrgID int64
	mustScan(t, pool, &newOrgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m18-new', 'M18 New', 1) RETURNING id")
	assertQuotas(t, pool, newOrgID, 0, 0, 0)

	if err := db.MigratePGTo(dsn, 17); err != nil {
		t.Fatalf("migrate down to 17: %v", err)
	}
	assertQuotas(t, pool, legacyID, 0, 0, 0)
	assertQuotas(t, pool, customID, 250000, 500000, 750000)

	var revertedDefaultID int64
	mustScan(t, pool, &revertedDefaultID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m18-reverted', 'M18 Reverted', 1) RETURNING id")
	assertQuotas(t, pool, revertedDefaultID, 100000, 1000000, 1000000)
}

func assertQuotas(t *testing.T, pool *pgxpool.Pool, orgID int64, wantTx, wantMetric, wantProfile int64) {
	t.Helper()
	var tx, metric, profile int64
	if err := pool.QueryRow(context.Background(),
		"SELECT transaction_quota, metric_quota, profile_quota FROM organizations WHERE id = $1", orgID).
		Scan(&tx, &metric, &profile); err != nil {
		t.Fatalf("select quotas org=%d: %v", orgID, err)
	}
	if tx != wantTx || metric != wantMetric || profile != wantProfile {
		t.Errorf("org=%d quotas = (%d,%d,%d), want (%d,%d,%d)", orgID, tx, metric, profile, wantTx, wantMetric, wantProfile)
	}
}
