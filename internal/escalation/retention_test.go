package escalation_test

import (
	"context"
	"testing"
	"time"

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

	deleted, err := escalation.PurgeOldEscalations(ctx, pool, 24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldEscalations: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("PurgeOldEscalations deleted = %d, want 1 (только осиротевшая строка)", deleted)
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
		if deleted, err := escalation.PurgeOldEscalations(ctx, pool, olderThan); err == nil {
			t.Fatalf("PurgeOldEscalations(olderThan=%s) = (%d, nil), want error", olderThan, deleted)
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
