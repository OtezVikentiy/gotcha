package trace_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

// TestRegressionNotifierEnqueuesPerChannel: открытие регрессии → по одной задаче
// в outbox на каждый включённый канал проекта, с корректным payload. Ключевая
// проверка — ловушка имён: адрес канала лежит под "target" (его читает
// notify.Worker), а имя цели регрессии — под "target_name", они не путаются.
func TestRegressionNotifierEnqueuesPerChannel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "regnotif1")
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

	n := &trace.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	ev := trace.RegressionEvent{
		Kind: "regression_open", ProjectID: pid, Target: "GET /api/users", Metric: "duration",
		BaselineValue: 800, CurrentValue: 1200, PctIncrease: 0.5,
	}
	if err := n.Notify(ctx, ev); err != nil {
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

	wantURL := "https://gotcha.example/projects/" + strconv.FormatInt(pid, 10) + "/regressions"

	j1, ok := byChannel[webhookCh]
	if !ok {
		t.Fatalf("no job for webhook channel %d", webhookCh)
	}
	if j1.Payload["kind"] != "regression_open" ||
		j1.Payload["project_id"] != float64(pid) ||
		j1.Payload["target_name"] != "GET /api/users" ||
		j1.Payload["metric"] != "duration" ||
		j1.Payload["baseline_value"] != float64(800) ||
		j1.Payload["current_value"] != float64(1200) ||
		j1.Payload["pct_increase"] != float64(0.5) ||
		j1.Payload["url"] != wantURL ||
		j1.Payload["channel_kind"] != alert.ChannelWebhook ||
		j1.Payload["target"] != "https://example.com/hook" {
		t.Errorf("webhook job payload = %+v", j1.Payload)
	}
	subject, _ := j1.Payload["subject"].(string)
	if !strings.Contains(subject, "GET /api/users") || !strings.Contains(subject, "+50%") {
		t.Errorf("subject = %q, want it to contain target and +50%%", subject)
	}
	body, ok := j1.Payload["body"].(string)
	if !ok || !strings.Contains(body, "GET /api/users") || !strings.Contains(body, wantURL) {
		t.Errorf("webhook job body = %v", j1.Payload["body"])
	}

	j2, ok := byChannel[telegramCh]
	if !ok {
		t.Fatalf("no job for telegram channel %d", telegramCh)
	}
	if j2.Payload["channel_kind"] != alert.ChannelTelegram ||
		j2.Payload["target"] != "123" ||
		j2.Payload["secret"] != "tok" ||
		j2.Payload["target_name"] != "GET /api/users" {
		t.Errorf("telegram job payload = %+v", j2.Payload)
	}
}

// TestRegressionNotifierCloseSubject: закрытие регрессии несёт свой заголовок и
// длительность инцидента — дежурному важно отличить «сломалось» от «починилось».
func TestRegressionNotifierCloseSubject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "regnotif-close")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	n := &trace.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	ev := trace.RegressionEvent{
		Kind: "regression_close", ProjectID: pid, Target: "LCP /", Metric: "lcp",
		BaselineValue: 2000, CurrentValue: 2100, PctIncrease: 0.05, DurationSeconds: 3665,
	}
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v err=%v, want 1", jobs, err)
	}
	subject, _ := jobs[0].Payload["subject"].(string)
	if !strings.Contains(subject, "устранена") || !strings.Contains(subject, "LCP /") || !strings.Contains(subject, "1h1m") {
		t.Errorf("close subject = %q, want it to mark resolution, target and duration", subject)
	}
}

// TestRegressionNotifierSkipsDisabledAndEmail: выключенный канал пропущен; email
// пропущен при EmailEnabled=false — остаётся ровно один включённый не-email канал.
func TestRegressionNotifierSkipsDisabledAndEmail(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "regnotif2")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com",
	}); err != nil {
		t.Fatalf("CreateChannel email: %v", err)
	}
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off",
	}); err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}
	enabledCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
	})
	if err != nil {
		t.Fatalf("CreateChannel telegram: %v", err)
	}

	n := &trace.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: false}
	ev := trace.RegressionEvent{
		Kind: "regression_open", ProjectID: pid, Target: "GET /x", Metric: "duration",
		BaselineValue: 100, CurrentValue: 250, PctIncrease: 1.5,
	}
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ChannelID != enabledCh {
		t.Fatalf("jobs = %+v err=%v, want exactly 1 job for channel %d", jobs, err, enabledCh)
	}
}

// TestRegressionNotifierNoChannels: проект без каналов → ничего не ставится, без
// ошибки.
func TestRegressionNotifierNoChannels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "regnotif3")
	n := &trace.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"}
	ev := trace.RegressionEvent{
		Kind: "regression_open", ProjectID: pid, Target: "GET /none", Metric: "duration",
		BaselineValue: 100, CurrentValue: 250, PctIncrease: 1.5,
	}
	if err := n.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("len(jobs) = %d, want 0", len(jobs))
	}
}
