package uptime_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// newNotifierMonitor создаёт монитор в проекте pid, привязанный к channelIDs
// (может быть пустым — тогда у монитора нет своих каналов).
func newNotifierMonitor(t *testing.T, svc *uptime.Service, pid int64, channelIDs []int64) uptime.Monitor {
	t.Helper()
	ctx := context.Background()
	m := baseHTTPMonitor(pid)
	m.Name = "API health"
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(ctx, m, []string{"local"}, channelIDs)
	if err != nil {
		t.Fatalf("Create monitor: %v", err)
	}
	return created
}

func TestOutboxNotifierOwnChannelsWebhookAndTelegram(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	webhookCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel webhook: %v", err)
	}
	telegramCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
	})
	if err != nil {
		t.Fatalf("CreateChannel telegram: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{webhookCh, telegramCh})

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	err = n.Notify(ctx, uptime.Event{
		Kind:    "down",
		Monitor: m,
		Regions: []string{"eu", "us"},
		Cause:   "connection refused",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("len(jobs) = %d, want 2", len(jobs))
	}
	byChannel := map[int64]notify.Job{}
	for _, j := range jobs {
		byChannel[j.ChannelID] = j
	}

	wantURL := "https://gotcha.example/monitors/" + strconv.FormatInt(m.ID, 10)
	wantSubject := "[Gotcha] API health is DOWN"

	j1, ok := byChannel[webhookCh]
	if !ok {
		t.Fatalf("no job for webhook channel %d", webhookCh)
	}
	if j1.Payload["kind"] != "down" ||
		j1.Payload["monitor_id"] != float64(m.ID) ||
		j1.Payload["monitor_name"] != "API health" ||
		j1.Payload["project_id"] != float64(pid) ||
		j1.Payload["cause"] != "connection refused" ||
		j1.Payload["url"] != wantURL ||
		j1.Payload["subject"] != wantSubject ||
		j1.Payload["channel_kind"] != alert.ChannelWebhook ||
		j1.Payload["target"] != "https://example.com/hook" {
		t.Errorf("webhook job payload = %+v", j1.Payload)
	}
	if _, ok := j1.Payload["body"].(string); !ok {
		t.Errorf("webhook job payload missing body: %+v", j1.Payload)
	}
	regions, ok := j1.Payload["regions"].([]any)
	if !ok || len(regions) != 2 || regions[0] != "eu" || regions[1] != "us" {
		t.Errorf("webhook job payload regions = %+v", j1.Payload["regions"])
	}

	j2, ok := byChannel[telegramCh]
	if !ok {
		t.Fatalf("no job for telegram channel %d", telegramCh)
	}
	if j2.Payload["channel_kind"] != alert.ChannelTelegram ||
		j2.Payload["target"] != "123" ||
		j2.Payload["secret"] != "tok" ||
		j2.Payload["subject"] != wantSubject {
		t.Errorf("telegram job payload = %+v", j2.Payload)
	}
}

func TestOutboxNotifierFallsBackToProjectChannels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	projectCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/project-hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Монитор без своих каналов.
	m := newNotifierMonitor(t, usvc, pid, nil)

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	if err := n.Notify(ctx, uptime.Event{Kind: "down", Monitor: m, Cause: "timeout"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ChannelID != projectCh {
		t.Fatalf("jobs = %+v err=%v, want exactly 1 job for project channel %d", jobs, err, projectCh)
	}
}

func TestOutboxNotifierSkipsEmailWhenDisabled(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	emailCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com",
	})
	if err != nil {
		t.Fatalf("CreateChannel email: %v", err)
	}
	webhookCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel webhook: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{emailCh, webhookCh})

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: false}
	if err := n.Notify(ctx, uptime.Event{Kind: "down", Monitor: m}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ChannelID != webhookCh {
		t.Fatalf("jobs = %+v err=%v, want exactly 1 job for webhook channel %d", jobs, err, webhookCh)
	}
}

func TestOutboxNotifierSkipsDisabledChannel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	disabledCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off",
	})
	if err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}
	enabledCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
	})
	if err != nil {
		t.Fatalf("CreateChannel enabled: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{disabledCh, enabledCh})

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	if err := n.Notify(ctx, uptime.Event{Kind: "down", Monitor: m}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ChannelID != enabledCh {
		t.Fatalf("jobs = %+v err=%v, want exactly 1 job for channel %d", jobs, err, enabledCh)
	}
}

// TestOutboxNotifierSubjectsPerKind проверяет форматы subject для всех
// видов Event через один и тот же канал.
func TestOutboxNotifierSubjectsPerKind(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	ch, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{ch})
	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example"}

	cases := []struct {
		ev      uptime.Event
		subject string
	}{
		{uptime.Event{Kind: "down", Monitor: m}, "[Gotcha] API health is DOWN"},
		{uptime.Event{Kind: "up", Monitor: m, DurationSeconds: 125}, "[Gotcha] API health is back UP (2m5s)"},
		{uptime.Event{Kind: "ssl_expiring", Monitor: m, DaysLeft: 7}, "[Gotcha] SSL for API health expires in 7 days"},
		{uptime.Event{Kind: "reminder", Monitor: m, DurationSeconds: 45}, "[Gotcha] API health still DOWN (45s)"},
	}
	for _, tc := range cases {
		t.Run(tc.ev.Kind, func(t *testing.T) {
			if err := n.Notify(ctx, tc.ev); err != nil {
				t.Fatalf("Notify: %v", err)
			}
			jobs, err := ob.Claim(ctx, 10)
			if err != nil || len(jobs) != 1 {
				t.Fatalf("jobs = %+v err=%v, want 1", jobs, err)
			}
			if jobs[0].Payload["subject"] != tc.subject {
				t.Errorf("subject = %v, want %q", jobs[0].Payload["subject"], tc.subject)
			}
			if err := ob.MarkSent(ctx, jobs[0].ID); err != nil {
				t.Fatalf("MarkSent: %v", err)
			}
		})
	}
}

func TestServiceMonitorChannelsOnlyEnabled(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	enabledCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/on",
	})
	if err != nil {
		t.Fatalf("CreateChannel enabled: %v", err)
	}
	disabledCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off",
	})
	if err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}
	// Канал проекта, не привязанный к монитору — не должен попасть в выборку.
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/unlinked",
	}); err != nil {
		t.Fatalf("CreateChannel unlinked: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{enabledCh, disabledCh})

	channels, err := usvc.MonitorChannels(ctx, m.ID)
	if err != nil {
		t.Fatalf("MonitorChannels: %v", err)
	}
	if len(channels) != 1 || channels[0].ID != enabledCh {
		t.Fatalf("MonitorChannels = %+v, want exactly [%d]", channels, enabledCh)
	}
}
