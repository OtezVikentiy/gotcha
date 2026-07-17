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

	n := &profile.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
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

// Трансграничный гейт: при ExternalDetails=false во внешние каналы не должно
// уезжать имя функции/сервиса (тело/subject); при true — уезжает.
func TestRegressionNotifierExternalDetailsGate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()

	ev := profile.ProfileRegressionEvent{
		Service: "api", ProfileType: "cpu", Function: "compress",
		BaselineShare: 0.1, CurrentShare: 0.3, PctIncrease: 2.0, Opened: true,
	}

	t.Run("withheld when false", func(t *testing.T) {
		pid := seedProject(t, pool)
		if _, err := asvc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("channel: %v", err)
		}
		n := &profile.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: false}
		e := ev
		e.ProjectID = pid
		if err := n.Notify(ctx, e); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		jobs, _ := ob.Claim(ctx, 10)
		if len(jobs) != 1 {
			t.Fatalf("jobs = %d, want 1", len(jobs))
		}
		p := jobs[0].Payload
		if _, ok := p["function"]; ok {
			t.Errorf("leaks function: %+v", p)
		}
		if body, _ := p["body"].(string); strings.Contains(body, "compress") || strings.Contains(body, "api") {
			t.Errorf("body leaks details: %q", body)
		}
		if subj, _ := p["subject"].(string); strings.Contains(subj, "compress") {
			t.Errorf("subject leaks function: %q", subj)
		}
		if p["url"] == nil {
			t.Errorf("lost url: %+v", p)
		}
	})

	t.Run("delivered when true", func(t *testing.T) {
		pid := seedProject(t, pool)
		if _, err := asvc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("channel: %v", err)
		}
		n := &profile.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
		e := ev
		e.ProjectID = pid
		if err := n.Notify(ctx, e); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		jobs, _ := ob.Claim(ctx, 10)
		if len(jobs) != 1 {
			t.Fatalf("jobs = %d, want 1", len(jobs))
		}
		if jobs[0].Payload["function"] != "compress" {
			t.Errorf("function missing at ExternalDetails=true: %+v", jobs[0].Payload)
		}
	})
}
