package trace_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestRegressionServiceEscalationName проверяет, что ключ источника
// совпадает с incident_source='trace', зафиксированным в миграции 0077.
func TestRegressionServiceEscalationName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	if got := svc.Name(); got != "trace" {
		t.Fatalf("Name() = %q, want %q", got, "trace")
	}
}

// TestRegressionServiceOpenUnacked дискриминирует «status='open' AND
// acknowledged_at IS NULL»: открытый неподтверждённый инцидент попадает в
// выборку с верными полями; после Acknowledge — пропадает; отдельный
// resolved-инцидент в выборку не попадает вовсе.
func TestRegressionServiceOpenUnacked(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx := context.Background()
	pid := newPerfProject(t, pool, "reg-esc")

	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /esc", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("OpenUnacked = %d/%v, want 1", len(list), err)
	}
	got := list[0]
	if got.ID != rec.ID || got.ProjectID != pid {
		t.Fatalf("OpenUnacked[0] = %+v, want ID=%d ProjectID=%d", got, rec.ID, pid)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("OpenUnacked[0].StartedAt пуст")
	}
	if got.Severity != "warning" {
		t.Fatalf("OpenUnacked[0].Severity = %q, want %q (trace-дефолт из 0077)", got.Severity, "warning")
	}
	if got.EscalationLevel != 0 {
		t.Fatalf("OpenUnacked[0].EscalationLevel = %d, want 0", got.EscalationLevel)
	}

	rec2, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /esc2", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open second: %v", err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 2 {
		t.Fatalf("OpenUnacked после второго open = %d/%v, want 2", len(list), err)
	}
	if ok, err := svc.Resolve(ctx, rec2.ID, 10); err != nil || !ok {
		t.Fatalf("Resolve(rec2): (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("OpenUnacked после Resolve = %+v/%v, want только первый регресс", list, err)
	}

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "trace-esc-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if ok, err := svc.Acknowledge(ctx, rec.ID, userID); err != nil || !ok {
		t.Fatalf("Acknowledge: (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("OpenUnacked после Acknowledge = %d/%v, want 0", len(list), err)
	}
}

// TestRegressionServiceBumpEscalation проверяет атомарность продвижения
// escalation_level: успешный бамп двигает level и last_escalated_at,
// повторный бамп с устаревшим from — идемпотентный no-op (ok=false).
func TestRegressionServiceBumpEscalation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := trace.NewRegressionService(pool)
	ctx := context.Background()
	pid := newPerfProject(t, pool, "reg-bump")

	rec, _, err := svc.Open(ctx, pid, "endpoint_p95", "GET /bump", "duration", 100, 250, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ok, err := svc.BumpEscalation(ctx, rec.ID, 0)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(0) = (%v,%v), want (true,nil)", ok, err)
	}
	var level int
	var lastEscalatedAt any
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level, last_escalated_at FROM perf_regressions WHERE id = $1", rec.ID).
		Scan(&level, &lastEscalatedAt); err != nil {
		t.Fatalf("select after bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после BumpEscalation(0) = %d, want 1", level)
	}
	if lastEscalatedAt == nil {
		t.Fatal("last_escalated_at после BumpEscalation(0) = nil, want заполнено")
	}

	ok, err = svc.BumpEscalation(ctx, rec.ID, 0)
	if err != nil || ok {
		t.Fatalf("повторный BumpEscalation(0) = (%v,%v), want (false,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM perf_regressions WHERE id = $1", rec.ID).Scan(&level); err != nil {
		t.Fatalf("select after stale bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после устаревшего BumpEscalation = %d, want 1 (не сдвинулся)", level)
	}

	ok, err = svc.BumpEscalation(ctx, rec.ID, 1)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(1) = (%v,%v), want (true,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM perf_regressions WHERE id = $1", rec.ID).Scan(&level); err != nil {
		t.Fatalf("select after second bump: %v", err)
	}
	if level != 2 {
		t.Fatalf("escalation_level после BumpEscalation(1) = %d, want 2", level)
	}
}
