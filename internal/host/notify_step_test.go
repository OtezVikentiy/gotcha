package host_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestHostNotifierStepDispatchesOnlyToChannelSet — NotifyStep(channelIDs) шлёт
// ТОЛЬКО в перечисленные каналы, даже если остальные deliverable, и пишет лог
// incident_escalations по каждому реально отправленному; disabled-канал не
// получает ничего независимо от channelIDs. Дискриминирует «в набор + лог» (T6).
func TestHostNotifierStepDispatchesOnlyToChannelSet(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "step-host")

	c1, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	c2, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"})
	if err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/disabled"}); err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, _, err := incidents.Open(ctx, projectID, h.ID, "disk", 0.95, "/var full", false)
	if err != nil {
		t.Fatalf("Open incident: %v", err)
	}

	n := &host.HostNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:   alert.NewDetailPolicy("", nil, true),
		Incidents: incidents, Hosts: host.NewStore(pool), Settings: host.NewSettingsService(pool),
		Pool: pool,
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
	if jobs[0].Payload["kind"] != "host_alert_open" {
		t.Errorf("kind = %v, want host_alert_open (эскалация повторяет open)", jobs[0].Payload["kind"])
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='host' AND incident_id=$1 AND channel_id=$2 AND step=2",
		in.ID, c1).Scan(&count); err != nil {
		t.Fatalf("select escalation log c1: %v", err)
	}
	if count != 1 {
		t.Fatalf("incident_escalations rows for c1/step2 = %d, want 1", count)
	}
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='host' AND incident_id=$1 AND channel_id=$2",
		in.ID, c2).Scan(&count); err != nil {
		t.Fatalf("select escalation log c2: %v", err)
	}
	if count != 0 {
		t.Fatalf("incident_escalations rows for c2 = %d, want 0 (c2 не в channelIDs)", count)
	}
}

// TestHostNotifierStepNilChannelIDsSendsToAllDeliverable — NotifyStep(nil) шлёт
// во ВСЕ deliverable-каналы проекта (старое поведение), с логом по каждому.
func TestHostNotifierStepNilChannelIDsSendsToAllDeliverable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "step-all-host")

	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"}); err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, _, err := incidents.Open(ctx, projectID, h.ID, "memory", 0.92, "", false)
	if err != nil {
		t.Fatalf("Open incident: %v", err)
	}

	n := &host.HostNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:   alert.NewDetailPolicy("", nil, true),
		Incidents: incidents, Hosts: host.NewStore(pool), Settings: host.NewSettingsService(pool),
		Pool: pool,
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
		"SELECT count(*) FROM incident_escalations WHERE incident_source='host' AND incident_id=$1 AND step=0",
		in.ID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 2 {
		t.Fatalf("incident_escalations rows for step0 = %d, want 2 (по одной на канал)", count)
	}
}

// TestHostNotifierRecoveryDispatchesWithoutLog — NotifyRecovery шлёт CLOSE-
// уведомление в ЗАДАННЫЙ канал и НЕ пишет incident_escalations (recovery не
// эскалирует).
func TestHostNotifierRecoveryDispatchesWithoutLog(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "recovery-host")

	c1, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, _, err := incidents.Open(ctx, projectID, h.ID, "load", 3.0, "", false)
	if err != nil {
		t.Fatalf("Open incident: %v", err)
	}

	n := &host.HostNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:   alert.NewDetailPolicy("", nil, true),
		Incidents: incidents, Hosts: host.NewStore(pool), Settings: host.NewSettingsService(pool),
		Pool: pool,
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
	if jobs[0].Payload["kind"] != "host_alert_resolved" {
		t.Errorf("kind = %v, want host_alert_resolved", jobs[0].Payload["kind"])
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='host' AND incident_id=$1",
		in.ID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Fatalf("incident_escalations rows = %d, want 0 (recovery не логирует)", count)
	}
}
