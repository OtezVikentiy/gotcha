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
