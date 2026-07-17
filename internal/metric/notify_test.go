package metric_test

import (
	"context"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestMetricNotifierEnqueues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}

	n := &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: false, ExternalDetails: true}
	ev := metric.MetricEvent{
		ProjectID: projectID, MetricName: "http.errors", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, Current: 250, Peak: 300, Environment: "prod", Opened: true,
	}
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify open: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	body, _ := jobs[0].Payload["body"].(string)
	if !strings.Contains(body, "http.errors") || !strings.Contains(body, "100") {
		t.Fatalf("body missing metric/threshold: %q", body)
	}
	if jobs[0].Payload["channel_kind"] != alert.ChannelWebhook {
		t.Fatalf("channel_kind = %v", jobs[0].Payload["channel_kind"])
	}

	// Закрытие тоже ставит задачу.
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
// уезжать имя метрики/значения (тело/subject); при true — уезжает.
func TestMetricNotifierExternalDetailsGate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()

	ev := metric.MetricEvent{
		MetricName: "http.errors", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, Current: 250, Peak: 300, Environment: "prod", Opened: true,
	}

	t.Run("withheld when false", func(t *testing.T) {
		pid := seedProject(t, pool)
		if _, err := asvc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("create channel: %v", err)
		}
		n := &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: false}
		e := ev
		e.ProjectID = pid
		if err := n.Notify(ctx, e); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
		}
		p := jobs[0].Payload
		if _, ok := p["metric"]; ok {
			t.Errorf("leaks metric name: %+v", p)
		}
		if body, _ := p["body"].(string); strings.Contains(body, "http.errors") {
			t.Errorf("body leaks metric: %q", body)
		}
		if subj, _ := p["subject"].(string); strings.Contains(subj, "http.errors") {
			t.Errorf("subject leaks metric: %q", subj)
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
			t.Fatalf("create channel: %v", err)
		}
		n := &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
		e := ev
		e.ProjectID = pid
		if err := n.Notify(ctx, e); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
		}
		if jobs[0].Payload["metric"] != "http.errors" {
			t.Errorf("metric missing at ExternalDetails=true: %+v", jobs[0].Payload)
		}
	})
}
