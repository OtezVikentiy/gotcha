package profile_test

import (
	"context"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRegressionNotifierStepDispatchesOnlyToChannelSet — NotifyStep(channelIDs)
// шлёт ТОЛЬКО в перечисленные каналы, даже если остальные deliverable, и
// возвращает их ID (реально заенкенные — то, что логирует ОРКЕСТРАЦИЯ,
// escalation.SendStepIfDue, см. TestSendStepIfDueLogsEnqueuedChannels в
// пакете escalation, T7-fix); disabled-канал не получает ничего независимо
// от channelIDs.
func TestRegressionNotifierStepDispatchesOnlyToChannelSet(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	c1, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/disabled"}); err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}

	regressions := profile.NewRegressionService(pool)
	r, _, err := regressions.Open(ctx, pid, "api", "cpu", "step", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("Open regression: %v", err)
	}

	n := &profile.RegressionNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:     alert.NewDetailPolicy("", nil, true),
		Regressions: regressions, Pool: pool,
	}

	enqueued, err := n.NotifyStep(ctx, r.ID, []int64{c1}, 2)
	if err != nil {
		t.Fatalf("NotifyStep: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0] != c1 {
		t.Fatalf("enqueued = %v, want [%d] (только c1)", enqueued, c1)
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
	if jobs[0].Payload["kind"] != "profile_regression_open" {
		t.Errorf("kind = %v, want profile_regression_open (эскалация повторяет open)", jobs[0].Payload["kind"])
	}
}

// TestRegressionNotifierStepNilChannelIDsSendsToAllDeliverable —
// NotifyStep(nil) шлёт во ВСЕ deliverable-каналы проекта (старое поведение)
// и возвращает их все как реально заенкенные.
func TestRegressionNotifierStepNilChannelIDsSendsToAllDeliverable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"}); err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	regressions := profile.NewRegressionService(pool)
	r, _, err := regressions.Open(ctx, pid, "api", "cpu", "step-all", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("Open regression: %v", err)
	}

	n := &profile.RegressionNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:     alert.NewDetailPolicy("", nil, true),
		Regressions: regressions, Pool: pool,
	}

	enqueued, err := n.NotifyStep(ctx, r.ID, nil, 0)
	if err != nil {
		t.Fatalf("NotifyStep: %v", err)
	}
	if len(enqueued) != 2 {
		t.Fatalf("enqueued = %v, want 2 channels (все deliverable)", enqueued)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 (все deliverable-каналы)", len(jobs))
	}
}

// TestRegressionNotifierRecoveryDispatchesWithoutLog — NotifyRecovery шлёт
// CLOSE-уведомление в ЗАДАННЫЙ канал и НЕ пишет incident_escalations
// (recovery не эскалирует).
func TestRegressionNotifierRecoveryDispatchesWithoutLog(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	c1, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	if err != nil {
		t.Fatalf("CreateChannel c1: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"}); err != nil {
		t.Fatalf("CreateChannel c2: %v", err)
	}

	regressions := profile.NewRegressionService(pool)
	r, _, err := regressions.Open(ctx, pid, "api", "cpu", "recovery", 0.1, 0.3, false)
	if err != nil {
		t.Fatalf("Open regression: %v", err)
	}

	n := &profile.RegressionNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details:     alert.NewDetailPolicy("", nil, true),
		Regressions: regressions, Pool: pool,
	}

	if err := n.NotifyRecovery(ctx, r.ID, []int64{c1}); err != nil {
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
	if jobs[0].Payload["kind"] != "profile_regression_resolved" {
		t.Errorf("kind = %v, want profile_regression_resolved", jobs[0].Payload["kind"])
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='profile' AND incident_id=$1",
		r.ID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 0 {
		t.Fatalf("incident_escalations rows = %d, want 0 (recovery не логирует)", count)
	}
}
