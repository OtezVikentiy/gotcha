package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// У Store нет Update, значит строки старше ErrBurnShortMinTooSmall поднять
// нечем кроме миграции; uptime-SLO не читает transactions_5m и не трогается.
func TestMigrate0098RaisesBurnShortMinFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 97); err != nil {
		t.Fatalf("migrate to 97: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m98', 'M98', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm98', 'M98') RETURNING id", orgID)

	var availID, latencyID, atFloorID, zeroID, uptimeID int64
	mustScan(t, pool, &availID,
		`INSERT INTO slos (project_id, name, sli_kind, target, window_days, burn_short_minutes)
		 VALUES ($1, 'avail-below', 'availability', 0.99, 30, 1) RETURNING id`, projectID)
	mustScan(t, pool, &latencyID,
		`INSERT INTO slos (project_id, name, sli_kind, target, window_days, threshold_ms, burn_short_minutes)
		 VALUES ($1, 'latency-below', 'latency', 0.99, 30, 500, 3) RETURNING id`, projectID)
	mustScan(t, pool, &atFloorID,
		`INSERT INTO slos (project_id, name, sli_kind, target, window_days, burn_short_minutes)
		 VALUES ($1, 'avail-at-floor', 'availability', 0.99, 30, 5) RETURNING id`, projectID)
	// 0 значит «умолчание» для Store.Create (не отклоняется), не «поднять до 5» —
	// миграция обязана оставить его как есть, тем же условием, что и создание.
	mustScan(t, pool, &zeroID,
		`INSERT INTO slos (project_id, name, sli_kind, target, window_days, burn_short_minutes)
		 VALUES ($1, 'avail-zero', 'availability', 0.99, 30, 0) RETURNING id`, projectID)
	var monitorID int64
	mustScan(t, pool, &monitorID,
		"INSERT INTO monitors (project_id, name, kind, interval_seconds) VALUES ($1, 'm', 'http', 60) RETURNING id",
		projectID)
	mustScan(t, pool, &uptimeID,
		`INSERT INTO slos (project_id, name, sli_kind, target, window_days, monitor_id, burn_short_minutes)
		 VALUES ($1, 'uptime-below', 'uptime', 0.99, 30, $2, 1) RETURNING id`, projectID, monitorID)

	if err := db.MigratePGTo(dsn, 98); err != nil {
		t.Fatalf("migrate to 98: %v", err)
	}

	wantFloor := map[int64]int{
		availID:   5, // ниже пола — поднят
		latencyID: 5, // ниже пола — поднят
		atFloorID: 5, // уже на полу — не тронут
		zeroID:    0, // 0 — «умолчание», не значение ниже пола — не тронут
		uptimeID:  1, // не SQL SLI — не тронут вовсе
	}
	for id, want := range wantFloor {
		var got int
		if err := pool.QueryRow(ctx,
			"SELECT burn_short_minutes FROM slos WHERE id=$1", id).Scan(&got); err != nil {
			t.Fatalf("select burn_short_minutes(%d): %v", id, err)
		}
		if got != want {
			t.Errorf("slo %d: burn_short_minutes = %d, want %d", id, got, want)
		}
	}

	if err := db.MigratePGTo(dsn, 97); err != nil {
		t.Fatalf("down to 97: %v", err)
	}
}
