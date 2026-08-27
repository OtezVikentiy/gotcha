package escalation_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestJanitorRunPurgesOldRows — Janitor.Run на каждом тике зовёт
// PurgeOldEscalations и удаляет строки incident_escalations старше Retention,
// не трогая свежие. Тик короткий; ctx.Done останавливает цикл. Дискриминирует:
// старая строка исчезает, свежая остаётся, а сам Run завершается по отмене
// контекста (без утечки горутины).
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

	// Старая строка (10 дней назад) — под удаление при Retention=24ч.
	if _, err := pool.Exec(ctx, `INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
		VALUES ('host', 7777, $1, 0, now() - interval '10 days')`, old); err != nil {
		t.Fatalf("insert old row: %v", err)
	}
	// Свежая строка — остаётся.
	if err := escalation.LogStep(ctx, pool, "host", 7778, fresh, 0); err != nil {
		t.Fatalf("LogStep fresh: %v", err)
	}

	j := &escalation.Janitor{Pool: pool, Retention: 24 * time.Hour, Interval: 10 * time.Millisecond}
	go j.Run(ctx)

	// Ждём, пока тик удалит старую строку (poll до ~3с).
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
	cancel() // останавливаем Run (ctx.Done ветка)

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

// TestPurgeOldEscalationsPurgesOldLogFailures — W2-C находка 3, ревью 2026-08-27:
// escalation_step_log_failures (миграция 0085) не имеет FK на incident_id,
// а её единственные писатели (recordLogFailure/clearLogFailure в
// SendStepIfDue) чистят строку только когда SendStepIfDue СНОВА позвали для
// той же тройки — инцидент, подтверждённый/закрытый раньше, оставляет
// строку осиротевшей навсегда без этой чистки. PurgeOldEscalations теперь
// удаляет и её строки старше olderThan (по last_attempt_at), той же
// ретенцией, что incident_escalations.
func TestPurgeOldEscalationsPurgesOldLogFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	// Осиротевшая строка (10 дней назад) — под удаление при olderThan=24ч.
	if _, err := pool.Exec(ctx, `
		INSERT INTO escalation_step_log_failures (incident_source, incident_id, step, attempts, last_attempt_at)
		VALUES ('host', 8881, 0, 3, now() - interval '10 days')`); err != nil {
		t.Fatalf("insert old log failure row: %v", err)
	}
	// Свежая строка — остаётся (инцидент ещё активно ретраит логирование).
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
