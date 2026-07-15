package profile_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRegressionNotifierEnqueues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}

	n := &profile.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	ev := profile.ProfileRegressionEvent{
		ProjectID: pid, Service: "api", ProfileType: "cpu", Function: "compress",
		BaselineShare: 0.1, CurrentShare: 0.3, PctIncrease: 2.0, Opened: true,
	}
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify open: %v", err)
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("open jobs = %d, want 1", len(jobs))
	}
	body, _ := jobs[0].Payload["body"].(string)
	if !strings.Contains(body, "compress") {
		t.Fatalf("body missing function: %q", body)
	}

	ev.Opened = false
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify close: %v", err)
	}
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 1 {
		t.Fatalf("close jobs = %d, want 1", len(jobs2))
	}
}
