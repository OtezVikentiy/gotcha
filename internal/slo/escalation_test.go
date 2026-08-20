package slo_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestStoreEscalationName проверяет, что ключ источника совпадает с
// incident_source='slo', зафиксированным в миграции 0077.
func TestStoreEscalationName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	st := slo.NewStore(pool)
	if got := st.Name(); got != "slo" {
		t.Fatalf("Name() = %q, want %q", got, "slo")
	}
}

func createSLO(t *testing.T, ctx context.Context, st *slo.Store, pid int64, name string) slo.SLO {
	t.Helper()
	def, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: name, Kind: slo.SLIAvailability, Target: 0.99,
		WindowDays: 30, BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create SLO %q: %v", name, err)
	}
	return def
}

// TestStoreOpenUnacked дискриминирует «status='open' AND acknowledged_at IS
// NULL»: открытый неподтверждённый инцидент попадает в выборку с верными
// полями; после Acknowledge — пропадает; отдельный resolved-инцидент в
// выборку не попадает вовсе.
func TestStoreOpenUnacked(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	def1 := createSLO(t, ctx, st, pid, "esc slo 1")
	def2 := createSLO(t, ctx, st, pid, "esc slo 2")

	rem := 0.5
	in, created, err := st.OpenIncident(ctx, def1.ID, pid, 20.0, &rem, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident(def1) = (%+v,%v,%v)", in, created, err)
	}

	list, err := st.OpenUnacked(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("OpenUnacked = %d/%v, want 1", len(list), err)
	}
	got := list[0]
	if got.ID != in.ID || got.ProjectID != pid {
		t.Fatalf("OpenUnacked[0] = %+v, want ID=%d ProjectID=%d", got, in.ID, pid)
	}
	if got.StartedAt.IsZero() {
		t.Fatal("OpenUnacked[0].StartedAt пуст")
	}
	if got.Severity != "critical" {
		t.Fatalf("OpenUnacked[0].Severity = %q, want %q (slo-дефолт из 0077)", got.Severity, "critical")
	}
	if got.EscalationLevel != 0 {
		t.Fatalf("OpenUnacked[0].EscalationLevel = %d, want 0", got.EscalationLevel)
	}

	in2, created, err := st.OpenIncident(ctx, def2.ID, pid, 15.0, &rem, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident(def2) = (%+v,%v,%v)", in2, created, err)
	}
	if list, err = st.OpenUnacked(ctx); err != nil || len(list) != 2 {
		t.Fatalf("OpenUnacked после второго open = %d/%v, want 2", len(list), err)
	}
	if _, ok, err := st.ResolveIncident(ctx, def2.ID); err != nil || !ok {
		t.Fatalf("ResolveIncident(def2): (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = st.OpenUnacked(ctx); err != nil || len(list) != 1 || list[0].ID != in.ID {
		t.Fatalf("OpenUnacked после ResolveIncident = %+v/%v, want только первый инцидент", list, err)
	}

	var userID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ($1,'x') RETURNING id", "slo-esc-ack@e.com").
		Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if ok, err := st.Acknowledge(ctx, in.ID, userID); err != nil || !ok {
		t.Fatalf("Acknowledge: (%v,%v), want (true,nil)", ok, err)
	}
	if list, err = st.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("OpenUnacked после Acknowledge = %d/%v, want 0", len(list), err)
	}
}

// TestStoreBumpEscalation проверяет атомарность продвижения
// escalation_level: успешный бамп двигает level и last_escalated_at,
// повторный бамп с устаревшим from — идемпотентный no-op (ok=false).
func TestStoreBumpEscalation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	def := createSLO(t, ctx, st, pid, "esc slo bump")
	rem := 0.5
	in, created, err := st.OpenIncident(ctx, def.ID, pid, 20.0, &rem, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident = (%+v,%v,%v)", in, created, err)
	}

	ok, err := st.BumpEscalation(ctx, in.ID, 0)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(0) = (%v,%v), want (true,nil)", ok, err)
	}
	var level int
	var lastEscalatedAt any
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level, last_escalated_at FROM slo_incidents WHERE id = $1", in.ID).
		Scan(&level, &lastEscalatedAt); err != nil {
		t.Fatalf("select after bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после BumpEscalation(0) = %d, want 1", level)
	}
	if lastEscalatedAt == nil {
		t.Fatal("last_escalated_at после BumpEscalation(0) = nil, want заполнено")
	}

	ok, err = st.BumpEscalation(ctx, in.ID, 0)
	if err != nil || ok {
		t.Fatalf("повторный BumpEscalation(0) = (%v,%v), want (false,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM slo_incidents WHERE id = $1", in.ID).Scan(&level); err != nil {
		t.Fatalf("select after stale bump: %v", err)
	}
	if level != 1 {
		t.Fatalf("escalation_level после устаревшего BumpEscalation = %d, want 1 (не сдвинулся)", level)
	}

	ok, err = st.BumpEscalation(ctx, in.ID, 1)
	if err != nil || !ok {
		t.Fatalf("BumpEscalation(1) = (%v,%v), want (true,nil)", ok, err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT escalation_level FROM slo_incidents WHERE id = $1", in.ID).Scan(&level); err != nil {
		t.Fatalf("select after second bump: %v", err)
	}
	if level != 2 {
		t.Fatalf("escalation_level после BumpEscalation(1) = %d, want 2", level)
	}
}
