package host_test

import (
	"context"
	"testing"
)

// TestIncidentServiceName проверяет, что ключ источника совпадает с
// incident_source='host', зафиксированным в миграции 0077.
func TestIncidentServiceName(t *testing.T) {
	_, svc, _, _ := setupIncidentHost(t)
	if got := svc.Name(); got != "host" {
		t.Fatalf("Name() = %q, want %q", got, "host")
	}
}

// TestIncidentServiceOpenUnacked дискриминирует «status='open' AND
// acknowledged_at IS NULL»: открытый неподтверждённый инцидент попадает в
// выборку с верными полями; после Acknowledge — пропадает; отдельный
// resolved-инцидент в выборку не попадает вовсе.
func TestIncidentServiceOpenUnacked(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	pending, created, err := svc.Open(ctx, projectID, hostID, "disk", 0.95, "/var full", false)
	if err != nil || !created {
		t.Fatalf("Open(disk): (%+v,%v,%v)", pending, created, err)
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("OpenUnacked до resolve/ack = %d записей, want 1: %+v", len(list), list)
	}
	got := list[0]
	if got.ID != pending.ID || got.ProjectID != projectID {
		t.Fatalf("OpenUnacked[0] = %+v, want ID=%d ProjectID=%d", got, pending.ID, projectID)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("OpenUnacked[0].StartedAt пуст")
	}
	if got.Severity != "critical" {
		t.Fatalf("OpenUnacked[0].Severity = %q, want %q (host-дефолт из 0077)", got.Severity, "critical")
	}
	if got.EscalationLevel != 0 {
		t.Fatalf("OpenUnacked[0].EscalationLevel = %d, want 0", got.EscalationLevel)
	}

	// Второй открытый инцидент другого вида — resolved не должен попасть в выборку.
	resolved, _, err := svc.Open(ctx, projectID, hostID, "memory", 0.9, "oom soon", false)
	if err != nil {
		t.Fatalf("Open(memory): %v", err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 2 {
		t.Fatalf("OpenUnacked после второго open = %d/%v, want 2", len(list), err)
	}
	if ok, err := svc.Resolve(ctx, resolved.ID, 0.5); err != nil || !ok {
		t.Fatalf("Resolve(memory): (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 1 || list[0].ID != pending.ID {
		t.Fatalf("OpenUnacked после Resolve = %+v/%v, want только disk-инцидент", list, err)
	}

	// Acknowledge гасит инцидент из выборки.
	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "host-esc-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if ok, err := svc.Acknowledge(ctx, pending.ID, projectID, userID); err != nil || !ok {
		t.Fatalf("Acknowledge: (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = svc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("OpenUnacked после Acknowledge = %d/%v, want 0", len(list), err)
	}
}

// TestIncidentServiceBumpEscalation проверяет атомарность продвижения
// escalation_level: успешный бамп двигает level и last_escalated_at,
// повторный бамп с устаревшим from — идемпотентный no-op (ok=false).
func TestIncidentServiceBumpEscalation(t *testing.T) {
	pool, svc, projectID, hostID := setupIncidentHost(t)
	ctx := context.Background()

	in, _, err := svc.Open(ctx, projectID, hostID, "load", 5.0, "load high", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	ok, err := svc.BumpEscalation(ctx, in.ID, 0)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(0) = (%v,%v), want (true,nil)", ok, err)
	}
	var level int
	var lastEscalatedAt any
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level, last_escalated_at FROM host_incidents WHERE id = $1", in.ID).
		Scan(&level, &lastEscalatedAt); err != nil {
		t.Fatalf("select after bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после BumpEscalation(0) = %d, want 1", level)
	}
	if lastEscalatedAt == nil {
		t.Fatal("last_escalated_at после BumpEscalation(0) = nil, want заполнено")
	}

	// Устаревший from (гонка/повтор тика) — ok=false, level не двигается.
	ok, err = svc.BumpEscalation(ctx, in.ID, 0)
	if err != nil || ok {
		t.Fatalf("повторный BumpEscalation(0) = (%v,%v), want (false,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM host_incidents WHERE id = $1", in.ID).Scan(&level); err != nil {
		t.Fatalf("select after stale bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после устаревшего BumpEscalation = %d, want 1 (не сдвинулся)", level)
	}

	// Следующий шаг с верным from — снова успех.
	ok, err = svc.BumpEscalation(ctx, in.ID, 1)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(1) = (%v,%v), want (true,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM host_incidents WHERE id = $1", in.ID).Scan(&level); err != nil {
		t.Fatalf("select after second bump: %v", err)
	}
	if level != 2 {
		t.Fatalf("escalation_level после BumpEscalation(1) = %d, want 2", level)
	}
}
