package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Открытый и уже отнотифаенный инцидент до миграции обязан получить escalation_level=1 и синтетическую
// строку в incident_escalations — иначе планировщик зашлёт step0 повторно, а recovery не найдёт лог.
func TestMigrate0077Escalations(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 76); err != nil {
		t.Fatalf("migrate to 76: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var org, proj int64
	mustScan(t, pool, &org,
		"INSERT INTO organizations (slug,name,event_quota) VALUES ('m77','M77',0) RETURNING id")
	mustScan(t, pool, &proj,
		"INSERT INTO projects (org_id,slug,name) VALUES ($1,'m77','M77') RETURNING id", org)

	var channelID int64
	mustScan(t, pool, &channelID,
		"INSERT INTO alert_channels (project_id,kind,enabled,target) VALUES ($1,'email',true,'a@b.c') RETURNING id",
		proj)

	// host-инцидент существует до миграции 77, открыт и уже отправил open-уведомление.
	var hostID int64
	mustScan(t, pool, &hostID,
		"INSERT INTO hosts (project_id,name) VALUES ($1,'h77') RETURNING id", proj)
	var hostIncidentID int64
	mustScan(t, pool, &hostIncidentID,
		`INSERT INTO host_incidents (project_id,host_id,kind,status,notified_open)
		 VALUES ($1,$2,'disk','open',true) RETURNING id`, proj, hostID)

	// metric-инцидент существует до миграции 77 — проверяет table-DEFAULT severity='warning'.
	var ruleID int64
	mustScan(t, pool, &ruleID,
		`INSERT INTO metric_alert_rules (project_id,metric_name,aggregation,comparator,threshold)
		 VALUES ($1,'cpu','avg','gt',90) RETURNING id`, proj)
	var metricIncidentID int64
	mustScan(t, pool, &metricIncidentID,
		`INSERT INTO metric_incidents (rule_id,project_id,status,peak_value,current_value,notified_open)
		 VALUES ($1,$2,'open',95,95,true) RETURNING id`, ruleID, proj)

	if err := db.MigratePGTo(dsn, 77); err != nil {
		t.Fatalf("migrate to 77: %v", err)
	}

	var level int
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM host_incidents WHERE id=$1", hostIncidentID).Scan(&level); err != nil {
		t.Fatalf("select escalation_level: %v", err)
	}
	if level != 1 {
		t.Fatalf("host_incidents.escalation_level = %d, want 1", level)
	}

	var hostSeverity string
	if err := pool.QueryRow(ctx,
		"SELECT severity FROM host_incidents WHERE id=$1", hostIncidentID).Scan(&hostSeverity); err != nil {
		t.Fatalf("select severity: %v", err)
	}
	if hostSeverity != "critical" {
		t.Fatalf("host_incidents.severity = %q, want critical", hostSeverity)
	}

	var metricSeverity string
	if err := pool.QueryRow(ctx,
		"SELECT severity FROM metric_incidents WHERE id=$1", metricIncidentID).Scan(&metricSeverity); err != nil {
		t.Fatalf("select severity: %v", err)
	}
	if metricSeverity != "warning" {
		t.Fatalf("metric_incidents.severity = %q, want warning", metricSeverity)
	}

	var loggedChannel int64
	var loggedStep int
	if err := pool.QueryRow(ctx,
		`SELECT channel_id, step FROM incident_escalations
		 WHERE incident_source='host' AND incident_id=$1`, hostIncidentID).Scan(&loggedChannel, &loggedStep); err != nil {
		t.Fatalf("select synthetic log: %v", err)
	}
	if loggedChannel != channelID || loggedStep != 0 {
		t.Fatalf("synthetic log = (channel %d, step %d), want (channel %d, step 0)", loggedChannel, loggedStep, channelID)
	}

	var metricLoggedStep int
	if err := pool.QueryRow(ctx,
		`SELECT step FROM incident_escalations
		 WHERE incident_source='metric' AND incident_id=$1`, metricIncidentID).Scan(&metricLoggedStep); err != nil {
		t.Fatalf("select synthetic log (metric): %v", err)
	}
	if metricLoggedStep != 0 {
		t.Fatalf("metric synthetic log step = %d, want 0", metricLoggedStep)
	}

	var stepsCount, stepChannelsCount int64
	mustScan(t, pool, &stepsCount, "SELECT count(*) FROM escalation_steps")
	if stepsCount != 0 {
		t.Fatalf("escalation_steps count = %d, want 0", stepsCount)
	}
	mustScan(t, pool, &stepChannelsCount, "SELECT count(*) FROM escalation_step_channels")
	if stepChannelsCount != 0 {
		t.Fatalf("escalation_step_channels count = %d, want 0", stepChannelsCount)
	}

	if err := db.MigratePGTo(dsn, 76); err != nil {
		t.Fatalf("down to 76: %v", err)
	}
}
