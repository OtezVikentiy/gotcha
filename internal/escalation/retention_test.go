package escalation_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestJanitorRunPurgesOldRows(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pid := newProject(t, pool)
	old := newChannel(t, pool, pid, true)
	fresh := newChannel(t, pool, pid, true)

	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 7777, $1, 0, now() - interval '10 days')`, old); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	if err := escalation.LogStep(ctx, pool, "host", 7778, fresh, 0); err != nil {
		t.Fatalf("LogStep fresh: %v", err)
	}

	j := &escalation.Janitor{Pool: pool, Retention: 24 * time.Hour, Interval: 10 * time.Millisecond}
	go j.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	var oldCount int
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM incident_escalations WHERE incident_id = 7777`).Scan(&oldCount); err != nil {
			t.Fatalf("count old: %v", err)
		}
		if oldCount == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	if oldCount != 0 {
		t.Fatalf("старая строка не удалена janitor'ом: count=%d", oldCount)
	}
	var freshCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM incident_escalations WHERE incident_id = 7778`).Scan(&freshCount); err != nil {
		t.Fatalf("count fresh: %v", err)
	}
	if freshCount != 1 {
		t.Fatalf("свежая строка не должна была удаляться: count=%d", freshCount)
	}
}

func TestPurgeOldEscalationsPurgesOldLogFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO escalation_step_log_failures (incident_source, incident_id, step, attempts, last_attempt_at)
		VALUES ('host', 8881, 0, 3, now() - interval '10 days')`); err != nil {
		t.Fatalf("insert old log failure row: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO escalation_step_log_failures (incident_source, incident_id, step, attempts, last_attempt_at)
		VALUES ('host', 8882, 0, 2, now())`); err != nil {
		t.Fatalf("insert fresh log failure row: %v", err)
	}

	res, err := escalation.PurgeOldEscalations(ctx, pool, 24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldEscalations: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("PurgeOldEscalations deleted = %d, want 1 (только осиротевшая строка)", res.Deleted)
	}

	var oldCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM escalation_step_log_failures WHERE incident_id = 8881").Scan(&oldCount); err != nil {
		t.Fatalf("count old: %v", err)
	}
	if oldCount != 0 {
		t.Fatalf("старая строка escalation_step_log_failures не удалена: count=%d", oldCount)
	}
	var freshCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM escalation_step_log_failures WHERE incident_id = 8882").Scan(&freshCount); err != nil {
		t.Fatalf("count fresh: %v", err)
	}
	if freshCount != 1 {
		t.Fatalf("свежая строка escalation_step_log_failures не должна была удаляться: count=%d", freshCount)
	}
}

func TestJanitorRunFirstPassIsImmediate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := newProject(t, pool)
	ch := newChannel(t, pool, pid, true)
	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 7779, $1, 0, now() - interval '10 days')`, ch); err != nil {
		t.Fatalf("insert old row: %v", err)
	}

	j := &escalation.Janitor{Pool: pool, Retention: 24 * time.Hour, Interval: time.Hour}
	jCtx, jCancel := context.WithCancel(ctx)
	defer jCancel()
	go j.Run(jCtx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM incident_escalations WHERE incident_id = 7779`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("старая строка не убрана за 5с — первого прохода нет, чистка ждёт целый Interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestJanitorTickLogsKeptOpenAndUnknownSource бьёт по обеим веткам логирования
// в tick через реальный Janitor.Run (не через прямой вызов PurgeOldEscalations):
// kept_open>0 — от старой строки открытого инцидента, и предупреждение
// о неизвестном источнике — от старой строки с несуществующим incident_source.
func TestJanitorTickLogsKeptOpenAndUnknownSource(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	buf := captureInfoLog(t)

	pid := newProjectNamed(t, pool, "esc-tick-log-org", "esc-tick-log")
	var hostID, incidentID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'host-1') RETURNING id", pid).Scan(&hostID); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO host_incidents
		(project_id, host_id, kind, status, current_value, peak_value, started_at, resolved_at)
		VALUES ($1, $2, 'disk', 'open', 0, 0, now() - interval '400 days', NULL)
		RETURNING id`, pid, hostID).Scan(&incidentID); err != nil {
		t.Fatalf("insert open incident: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations
		(incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', $1, 1, 1, now() - interval '400 days')`, incidentID); err != nil {
		t.Fatalf("insert escalation row: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations
		(incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('webhook', 5552, 1, 1, now() - interval '400 days')`); err != nil {
		t.Fatalf("insert unknown-source escalation row: %v", err)
	}

	j := &escalation.Janitor{Pool: pool, Retention: 90 * 24 * time.Hour, Interval: time.Hour}
	jCtx, jCancel := context.WithCancel(ctx)
	defer jCancel()
	go j.Run(jCtx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(buf.String(), "kept_open=1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("лог kept_open не появился за 5с: %s", buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	jCancel()

	log := buf.String()
	if !strings.Contains(log, "escalation janitor: purged old rows") || !strings.Contains(log, "deleted=1") {
		t.Fatalf("не найдено сообщение о deleted/kept_open в логе тика: %s", log)
	}
	if !strings.Contains(log, "escalation janitor: purged rows with unknown incident source by age") ||
		!strings.Contains(log, "webhook") {
		t.Fatalf("не найдено сообщение о неизвестном источнике в логе тика: %s", log)
	}
}

func TestPurgeKeepsEscalationsOfOpenIncident(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)

	pid := newProjectNamed(t, pool, "esc-open-org", "esc-open")
	var hostID, incidentID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO hosts (project_id, name) VALUES ($1, 'host-1') RETURNING id", pid).Scan(&hostID); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO host_incidents
		(project_id, host_id, kind, status, current_value, peak_value, started_at, resolved_at)
		VALUES ($1, $2, 'disk', 'open', 0, 0, now() - interval '400 days', NULL)
		RETURNING id`, pid, hostID).Scan(&incidentID); err != nil {
		t.Fatalf("insert open incident: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations
		(incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', $1, 1, 1, now() - interval '400 days')`, incidentID); err != nil {
		t.Fatalf("insert escalation row: %v", err)
	}

	res, err := escalation.PurgeOldEscalations(ctx, pool, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("удалено %d строк журнала открытого инцидента, ожидалось 0", res.Deleted)
	}
	if res.KeptOpen != 1 {
		t.Fatalf("KeptOpen = %d, ожидалась 1 сохранённая строка открытого инцидента", res.KeptOpen)
	}
}

// escSeed заводит родительские записи (хост/правило/SLO/монитор — по источнику)
// и саму строку инцидента; resolvedAt == nil означает открытый инцидент.
type escSeed func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64

func seedHostIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64 {
	t.Helper()
	var hostID, incidentID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO hosts (project_id, name) VALUES ($1, $2) RETURNING id", projectID, name).Scan(&hostID); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	status := "open"
	if resolvedAt != nil {
		status = "resolved"
	}
	if err := pool.QueryRow(ctx, `INSERT INTO host_incidents
		(project_id, host_id, kind, status, current_value, peak_value, started_at, resolved_at)
		VALUES ($1, $2, 'disk', $3, 0, 0, now() - interval '400 days', $4) RETURNING id`,
		projectID, hostID, status, resolvedAt).Scan(&incidentID); err != nil {
		t.Fatalf("insert host_incidents: %v", err)
	}
	return incidentID
}

func seedMetricIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64 {
	t.Helper()
	var ruleID, incidentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO metric_alert_rules
		(project_id, metric_name, aggregation, comparator, threshold) VALUES ($1, $2, 'avg', 'gt', 90) RETURNING id`,
		projectID, name).Scan(&ruleID); err != nil {
		t.Fatalf("insert metric_alert_rules: %v", err)
	}
	status := "open"
	if resolvedAt != nil {
		status = "resolved"
	}
	if err := pool.QueryRow(ctx, `INSERT INTO metric_incidents
		(rule_id, project_id, status, peak_value, current_value, started_at, resolved_at)
		VALUES ($1, $2, $3, 10, 10, now() - interval '400 days', $4) RETURNING id`,
		ruleID, projectID, status, resolvedAt).Scan(&incidentID); err != nil {
		t.Fatalf("insert metric_incidents: %v", err)
	}
	return incidentID
}

func seedTraceRegression(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64 {
	t.Helper()
	status := "open"
	if resolvedAt != nil {
		status = "resolved"
	}
	var incidentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO perf_regressions
		(project_id, target_kind, target, metric, status, baseline_value, peak_value, current_value, started_at, resolved_at)
		VALUES ($1, 'endpoint_p95', $2, 'duration', $3, 10, 20, 20, now() - interval '400 days', $4) RETURNING id`,
		projectID, name, status, resolvedAt).Scan(&incidentID); err != nil {
		t.Fatalf("insert perf_regressions: %v", err)
	}
	return incidentID
}

func seedProfileRegression(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64 {
	t.Helper()
	status := "open"
	if resolvedAt != nil {
		status = "resolved"
	}
	var incidentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO profile_regressions
		(project_id, service, profile_type, function, status, baseline_share, peak_share, current_share, started_at, resolved_at)
		VALUES ($1, 'svc', 'cpu', $2, $3, 0.1, 0.2, 0.2, now() - interval '400 days', $4) RETURNING id`,
		projectID, name, status, resolvedAt).Scan(&incidentID); err != nil {
		t.Fatalf("insert profile_regressions: %v", err)
	}
	return incidentID
}

func seedSLOIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64 {
	t.Helper()
	var sloID, incidentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO slos
		(project_id, name, sli_kind, target, window_days) VALUES ($1, $2, 'availability', 0.99, 30) RETURNING id`,
		projectID, name).Scan(&sloID); err != nil {
		t.Fatalf("insert slos: %v", err)
	}
	status := "open"
	if resolvedAt != nil {
		status = "resolved"
	}
	if err := pool.QueryRow(ctx, `INSERT INTO slo_incidents
		(slo_id, project_id, status, burn_rate, started_at, resolved_at)
		VALUES ($1, $2, $3, 5, now() - interval '400 days', $4) RETURNING id`,
		sloID, projectID, status, resolvedAt).Scan(&incidentID); err != nil {
		t.Fatalf("insert slo_incidents: %v", err)
	}
	return incidentID
}

func seedUptimeIncident(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID int64, name string, resolvedAt *time.Time) int64 {
	t.Helper()
	var monitorID, incidentID int64
	if err := pool.QueryRow(ctx, `INSERT INTO monitors
		(project_id, name, kind, interval_seconds) VALUES ($1, $2, 'http', 60) RETURNING id`,
		projectID, name).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitors: %v", err)
	}
	// incidents (uptime) не имеет колонки status — закрытость только через resolved_at.
	if err := pool.QueryRow(ctx, `INSERT INTO incidents
		(monitor_id, started_at, resolved_at) VALUES ($1, now() - interval '400 days', $2) RETURNING id`,
		monitorID, resolvedAt).Scan(&incidentID); err != nil {
		t.Fatalf("insert incidents: %v", err)
	}
	return incidentID
}

// TestPurgeEscalationsBySourceAndCloseness итерирует все шесть источников инцидентов
// и для каждого проверяет три ветки: открытый инцидент (строка держится), закрытый
// раньше cutoff (строка чистится), инцидент отсутствует в своей таблице (строка чистится
// по возрасту как осиротевшая).
func TestPurgeEscalationsBySourceAndCloseness(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)
	pid := newProjectNamed(t, pool, "esc-src-org", "esc-src")

	cases := []struct {
		source string
		seed   escSeed
	}{
		{"host", seedHostIncident},
		{"metric", seedMetricIncident},
		{"trace", seedTraceRegression},
		{"profile", seedProfileRegression},
		{"slo", seedSLOIncident},
		{"uptime", seedUptimeIncident},
	}

	closedAt := time.Now().Add(-200 * 24 * time.Hour) // раньше cutoff (90 дней)
	type row struct {
		source     string
		branch     string
		escID      int64
		wantDelete bool
	}
	var rows []row

	insertEscalation := func(source string, incidentID int64) int64 {
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO incident_escalations
			(incident_source, incident_id, channel_id, step, sent_at)
			VALUES ($1, $2, 1, 1, now() - interval '400 days') RETURNING id`,
			source, incidentID).Scan(&id); err != nil {
			t.Fatalf("insert incident_escalations(%s): %v", source, err)
		}
		return id
	}

	for _, c := range cases {
		openID := c.seed(t, ctx, pool, pid, c.source+"-open", nil)
		rows = append(rows, row{c.source, "open", insertEscalation(c.source, openID), false})

		closedID := c.seed(t, ctx, pool, pid, c.source+"-closed", &closedAt)
		rows = append(rows, row{c.source, "closed", insertEscalation(c.source, closedID), true})

		missingID := openID + closedID + 1_000_000 // заведомо не существующий id в таблице источника
		rows = append(rows, row{c.source, "missing", insertEscalation(c.source, missingID), true})
	}

	res, err := escalation.PurgeOldEscalations(ctx, pool, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.KeptOpen != int64(len(cases)) {
		t.Fatalf("KeptOpen = %d, want %d (по одной открытой строке на источник)", res.KeptOpen, len(cases))
	}

	for _, r := range rows {
		t.Run(r.source+"/"+r.branch, func(t *testing.T) {
			var n int
			if err := pool.QueryRow(ctx,
				"SELECT count(*) FROM incident_escalations WHERE id = $1", r.escID).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			exists := n == 1
			if r.wantDelete && exists {
				t.Fatalf("строка источника %s (%s) не удалена, хотя ожидалась чистка по возрасту", r.source, r.branch)
			}
			if !r.wantDelete && !exists {
				t.Fatalf("строка источника %s (%s) удалена, хотя её инцидент открыт", r.source, r.branch)
			}
		})
	}
}

func TestPurgeOldEscalationsUnknownSourcePurgedByAge(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	ctx := context.Background()
	pool := testenv.MigratedPG(t)

	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations
		(incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('webhook', 5551, 1, 1, now() - interval '400 days')`); err != nil {
		t.Fatalf("insert escalation row: %v", err)
	}

	res, err := escalation.PurgeOldEscalations(ctx, pool, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1 (строка неизвестного источника чистится по возрасту)", res.Deleted)
	}
	if len(res.UnknownSources) != 1 || res.UnknownSources[0] != "webhook" {
		t.Fatalf("UnknownSources = %v, want [webhook]", res.UnknownSources)
	}
}

func TestPurgeOldEscalationsRejectsNonPositiveRetention(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := newProject(t, pool)
	ch := newChannel(t, pool, pid, true)
	if err := escalation.LogStep(ctx, pool, "host", 9991, ch, 0); err != nil {
		t.Fatalf("LogStep: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO escalation_step_log_failures (incident_source, incident_id, step, attempts, last_attempt_at)
		VALUES ('host', 9992, 0, 1, now())`); err != nil {
		t.Fatalf("insert log failure row: %v", err)
	}

	for _, olderThan := range []time.Duration{0, -time.Hour} {
		if res, err := escalation.PurgeOldEscalations(ctx, pool, olderThan); err == nil {
			t.Fatalf("PurgeOldEscalations(olderThan=%s) = (%+v, nil), want error", olderThan, res)
		}
	}

	var escCount, failCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_id = 9991").Scan(&escCount); err != nil {
		t.Fatalf("count incident_escalations: %v", err)
	}
	if escCount != 1 {
		t.Fatalf("incident_escalations удалены при отклонённом вызове: count=%d, want 1", escCount)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM escalation_step_log_failures WHERE incident_id = 9992").Scan(&failCount); err != nil {
		t.Fatalf("count escalation_step_log_failures: %v", err)
	}
	if failCount != 1 {
		t.Fatalf("escalation_step_log_failures удалены при отклонённом вызове: count=%d, want 1", failCount)
	}
}
