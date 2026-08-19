package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0072CreatesSLOs — таблицы slos и slo_incidents создаются на базе,
// где уже есть организация и проект; принимают строки с FK на проект и SLO,
// заполняют DEFAULT-поля (burn_threshold=14.4, окна burn, enabled) и уходят при
// откате (DROP TABLE), не задев проект.
func TestMigrate0072CreatesSLOs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 71); err != nil {
		t.Fatalf("migrate to 71: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID int64
	mustScan(t, pool, &orgID,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ('m72', 'M72', 0) RETURNING id")
	mustScan(t, pool, &projectID,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm72', 'M72') RETURNING id", orgID)

	if err := db.MigratePGTo(dsn, 72); err != nil {
		t.Fatalf("migrate to 72: %v", err)
	}

	// SLO с FK на проект; DEFAULT-поля заполняются без явных значений.
	var sloID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO slos (project_id, name, sli_kind, target, window_days) VALUES ($1, 'checkout', 'availability', 0.99, 30) RETURNING id",
		projectID).Scan(&sloID); err != nil {
		t.Fatalf("insert slo после миграции: %v", err)
	}
	if sloID == 0 {
		t.Fatal("slo id = 0")
	}
	var burnThreshold float64
	var burnLong, burnShort, thresholdMS int
	var enabled bool
	var transaction, environment string
	if err := pool.QueryRow(ctx,
		"SELECT burn_threshold, burn_long_minutes, burn_short_minutes, threshold_ms, enabled, transaction, environment FROM slos WHERE id = $1", sloID).
		Scan(&burnThreshold, &burnLong, &burnShort, &thresholdMS, &enabled, &transaction, &environment); err != nil {
		t.Fatalf("select slo: %v", err)
	}
	if burnThreshold != 14.4 || burnLong != 60 || burnShort != 5 || thresholdMS != 0 || !enabled || transaction != "" || environment != "" {
		t.Fatalf("DEFAULT-поля slos = (%v, %d, %d, %d, %v, %q, %q), want (14.4, 60, 5, 0, true, \"\", \"\")",
			burnThreshold, burnLong, burnShort, thresholdMS, enabled, transaction, environment)
	}

	// Инцидент с FK на SLO и проект; DEFAULT status='open', флаги notified false.
	var incID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO slo_incidents (slo_id, project_id, burn_rate) VALUES ($1, $2, 20.0) RETURNING id",
		sloID, projectID).Scan(&incID); err != nil {
		t.Fatalf("insert slo_incident после миграции: %v", err)
	}
	var status string
	var notifiedOpen, notifiedClose bool
	if err := pool.QueryRow(ctx,
		"SELECT status, notified_open, notified_close FROM slo_incidents WHERE id = $1", incID).
		Scan(&status, &notifiedOpen, &notifiedClose); err != nil {
		t.Fatalf("select slo_incident: %v", err)
	}
	if status != "open" || notifiedOpen || notifiedClose {
		t.Fatalf("DEFAULT-поля slo_incidents = (%q, %v, %v), want (open, false, false)", status, notifiedOpen, notifiedClose)
	}

	// Откат — DROP TABLE обеих таблиц; проект обязан уцелеть.
	if err := db.MigratePGTo(dsn, 71); err != nil {
		t.Fatalf("migrate down to 71: %v", err)
	}
	for _, table := range []string{"slos", "slo_incidents"} {
		var exists bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)`, table).Scan(&exists); err != nil {
			t.Fatalf("check %s table: %v", table, err)
		}
		if exists {
			t.Fatalf("таблица %s должна исчезнуть после отката 0072", table)
		}
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM projects WHERE id = $1", projectID).Scan(&name); err != nil {
		t.Fatalf("project row must survive down-migration: %v", err)
	}
	if name != "M72" {
		t.Fatalf("project name = %q, want M72", name)
	}
}
