package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestMigrate0084BackfillsUptimeEscalationState — миграция 0084 (W2-C
// находка 2 аудита 2026-08-27) добавляет incidents (uptime) те же колонки,
// что 0077 дала пяти остальным инцидент-таблицам, и — как и 0077 —
// бэкафилливает существующие открытые+отнотифаенные инциденты: они уже
// отправили open-уведомление ДО появления эскалаций, значит ступень 0
// фактически состоялась, и планировщик не должен зашить её заново
// (escalation_level=1) или молчать на recovery (синтетический step0-лог).
// Строка здесь заводится ДО миграции (schema-версия 83), ровно как её увидит
// любая работающая инсталляция при апгрейде.
func TestMigrate0084BackfillsUptimeEscalationState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePGTo(dsn, 83); err != nil {
		t.Fatalf("migrate to 83: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	var orgID, projectID, monitorID, channelID, incidentID int64
	mustScan(t, pool, &orgID, "INSERT INTO organizations (slug, name) VALUES ('m84', 'M84') RETURNING id")
	mustScan(t, pool, &projectID, "INSERT INTO projects (org_id, slug, name) VALUES ($1, 'm84', 'M84') RETURNING id", orgID)
	mustScan(t, pool, &channelID, `
		INSERT INTO alert_channels (project_id, kind, target, enabled)
		VALUES ($1, 'webhook', 'https://example.com/hook', true) RETURNING id`, projectID)
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1, 'm84-monitor', 'heartbeat', 60) RETURNING id`, projectID)
	// Открытый, уже отнотифаенный инцидент — то самое доэскалационное
	// состояние, которое 0084 обязана унаследовать как "шаг 0 состоялся".
	mustScan(t, pool, &incidentID, `
		INSERT INTO incidents (monitor_id, cause, notified_open)
		VALUES ($1, 'connection refused', true) RETURNING id`, monitorID)
	// Закрытый несвязанный инцидент — контрольная строка: backfill не должен
	// трогать resolved-инциденты (миграция 0077 фильтрует по "status='open'"
	// у остальных таблиц, incidents эквивалент — resolved_at IS NULL).
	var closedIncidentID int64
	mustScan(t, pool, &closedIncidentID, `
		INSERT INTO incidents (monitor_id, cause, notified_open, resolved_at)
		VALUES ($1, 'blip', true, now()) RETURNING id`, monitorID)

	if err := db.MigratePGTo(dsn, 84); err != nil {
		t.Fatalf("migrate to 84: %v", err)
	}

	var severity string
	var escalationLevel int
	var ackAt any
	var notifyFailed bool
	var notifyAttempts int
	if err := pool.QueryRow(ctx,
		"SELECT severity, escalation_level, acknowledged_at, notify_open_failed, notify_open_attempts FROM incidents WHERE id = $1",
		incidentID).Scan(&severity, &escalationLevel, &ackAt, &notifyFailed, &notifyAttempts); err != nil {
		t.Fatalf("select migrated incident: %v", err)
	}
	if severity != "critical" {
		t.Errorf("severity = %q, want critical (default)", severity)
	}
	if escalationLevel != 1 {
		t.Errorf("escalation_level = %d, want 1 (backfill: already-notified open incident counts as step 0 done)", escalationLevel)
	}
	if ackAt != nil {
		t.Errorf("acknowledged_at = %v, want NULL", ackAt)
	}
	if notifyFailed {
		t.Error("notify_open_failed = true, want false (default)")
	}
	if notifyAttempts != 0 {
		t.Errorf("notify_open_attempts = %d, want 0 (default)", notifyAttempts)
	}

	var closedLevel int
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM incidents WHERE id = $1", closedIncidentID).Scan(&closedLevel); err != nil {
		t.Fatalf("select closed incident: %v", err)
	}
	if closedLevel != 0 {
		t.Errorf("closed incident escalation_level = %d, want 0 (backfill only touches open incidents)", closedLevel)
	}

	// Синтетический step0-лог: без него RecoveryChannels ничего не найдёт
	// для этого инцидента, и recovery ("up") не сможет адресоваться никуда.
	var logged int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1 AND channel_id=$2 AND step=0",
		incidentID, channelID).Scan(&logged); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if logged != 1 {
		t.Errorf("synthetic step0 log rows = %d, want 1 (incident=%d channel=%d)", logged, incidentID, channelID)
	}

	var closedLogged int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1",
		closedIncidentID).Scan(&closedLogged); err != nil {
		t.Fatalf("select incident_escalations for closed incident: %v", err)
	}
	if closedLogged != 0 {
		t.Errorf("synthetic step0 log rows for closed incident = %d, want 0", closedLogged)
	}
}
