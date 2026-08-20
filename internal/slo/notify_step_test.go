package slo_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestSLOBurnNotifierStepDispatchesOnlyToChannelSet — NotifyStep(channelIDs)
// шлёт ТОЛЬКО в перечисленные каналы, даже если остальные deliverable, и
// пишет лог incident_escalations по каждому реально отправленному;
// disabled-канал не получает ничего независимо от channelIDs.
func TestSLOBurnNotifierStepDispatchesOnlyToChannelSet(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	c1, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	c2, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"})
	if err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/disabled"}); err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}

	def := createSLO(t, ctx, st, pid, "step slo")
	rem := 0.5
	in, _, err := st.OpenIncident(ctx, def.ID, pid, 20.0, &rem, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	n := &slo.SLOBurnNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true),
		Store:   st, Pool: pool,
	}

	if err := n.NotifyStep(ctx, in.ID, []int64{c1}, 2); err != nil {
		t.Fatalf("NotifyStep: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (только c1)", len(jobs))
	}
	if jobs[0].ChannelID != c1 {
		t.Fatalf("job channel = %d, want %d (c1)", jobs[0].ChannelID, c1)
	}
	if jobs[0].Payload["kind"] != "slo_burn_open" {
		t.Errorf("kind = %v, want slo_burn_open (эскалация повторяет open)", jobs[0].Payload["kind"])
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='slo' AND incident_id=$1 AND channel_id=$2 AND step=2",
		in.ID, c1).Scan(&count); err != nil {
		t.Fatalf("select escalation log c1: %v", err)
	}
	if count != 1 {
		t.Fatalf("incident_escalations rows for c1/step2 = %d, want 1", count)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='slo' AND incident_id=$1 AND channel_id=$2",
		in.ID, c2).Scan(&count); err != nil {
		t.Fatalf("select escalation log c2: %v", err)
	}
	if count != 0 {
		t.Fatalf("incident_escalations rows for c2 = %d, want 0 (c2 не в channelIDs)", count)
	}
}

// TestSLOBurnNotifierStepNilChannelIDsSendsToAllDeliverable —
// NotifyStep(nil) шлёт во ВСЕ deliverable-каналы проекта (старое поведение),
// с логом по каждому.
func TestSLOBurnNotifierStepNilChannelIDsSendsToAllDeliverable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"}); err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	def := createSLO(t, ctx, st, pid, "step-all slo")
	rem := 0.5
	in, _, err := st.OpenIncident(ctx, def.ID, pid, 20.0, &rem, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	n := &slo.SLOBurnNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true),
		Store:   st, Pool: pool,
	}

	if err := n.NotifyStep(ctx, in.ID, nil, 0); err != nil {
		t.Fatalf("NotifyStep: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 (все deliverable-каналы)", len(jobs))
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='slo' AND incident_id=$1 AND step=0",
		in.ID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 2 {
		t.Fatalf("incident_escalations rows for step0 = %d, want 2 (по одной на канал)", count)
	}
}

// TestSLOBurnNotifierRecoveryDispatchesWithoutLog — NotifyRecovery шлёт
// CLOSE-уведомление в ЗАДАННЫЙ канал и НЕ пишет incident_escalations
// (recovery не эскалирует).
func TestSLOBurnNotifierRecoveryDispatchesWithoutLog(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)

	c1, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	def := createSLO(t, ctx, st, pid, "recovery slo")
	rem := 0.5
	in, _, err := st.OpenIncident(ctx, def.ID, pid, 20.0, &rem, false)
	if err != nil {
		t.Fatalf("OpenIncident: %v", err)
	}

	n := &slo.SLOBurnNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true),
		Store:   st, Pool: pool,
	}

	if err := n.NotifyRecovery(ctx, in.ID, []int64{c1}); err != nil {
		t.Fatalf("NotifyRecovery: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1 (только c1)", len(jobs))
	}
	if jobs[0].ChannelID != c1 {
		t.Fatalf("job channel = %d, want %d (c1)", jobs[0].ChannelID, c1)
	}
	if jobs[0].Payload["kind"] != "slo_burn_close" {
		t.Errorf("kind = %v, want slo_burn_close", jobs[0].Payload["kind"])
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='slo' AND incident_id=$1",
		in.ID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Fatalf("incident_escalations rows = %d, want 0 (recovery не логирует)", count)
	}
}
