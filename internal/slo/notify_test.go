package slo_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestSLOBurnNotifierEnqueues: открытие инцидента сжигания бюджета → по одной
// задаче в outbox на каждый включённый канал проекта, с корректным payload.
// Ключевая проверка — ловушка имён: адрес канала лежит под "target" (его читает
// notify.Worker), а имя SLO — под "target_name".
func TestSLOBurnNotifierEnqueues(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := seedProject(t, pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	webhookCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	n := &slo.SLOBurnNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "ru"},
	}
	ev := slo.SLOEvent{
		SLO:        slo.SLO{ID: 7, ProjectID: pid, Name: "checkout", Kind: slo.SLIAvailability},
		Opened:     true,
		Attainment: 0.98, BudgetRemaining: 0.3, BurnRate: 20,
	}
	n.Notify(ctx, ev)

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1", len(jobs))
	}
	p := jobs[0].Payload
	if jobs[0].ChannelID != webhookCh {
		t.Errorf("job channel = %d, want %d", jobs[0].ChannelID, webhookCh)
	}
	if p["kind"] != "slo_burn" ||
		p["channel_kind"] != alert.ChannelWebhook ||
		p["target"] != "https://example.com/hook" ||
		p["target_name"] != "checkout" {
		t.Errorf("payload = %+v", p)
	}
	wantURL := "https://gotcha.example/projects/" + strconv.FormatInt(pid, 10) + "/slos/7"
	if p["url"] != wantURL {
		t.Errorf("url = %v, want %v", p["url"], wantURL)
	}
	subject, _ := p["subject"].(string)
	if !strings.Contains(subject, "checkout") {
		t.Errorf("subject = %q, want it to contain the SLO name", subject)
	}
	body, _ := p["body"].(string)
	if !strings.Contains(body, "checkout") || !strings.Contains(body, wantURL) {
		t.Errorf("body = %q, want name and url", body)
	}
}

// TestSLOBurnNotifierExternalRedaction: при политике без доверия получателю во
// внешний канал не уезжает имя SLO (target_name/тело), но остаётся url.
func TestSLOBurnNotifierExternalRedaction(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := seedProject(t, pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	n := &slo.SLOBurnNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, false), Locale: i18n.Locale{Code: "ru"},
	}
	ev := slo.SLOEvent{
		SLO:        slo.SLO{ID: 3, ProjectID: pid, Name: "secret-checkout", Kind: slo.SLIAvailability},
		Opened:     true,
		Attainment: 0.98, BudgetRemaining: 0.3, BurnRate: 20,
	}
	n.Notify(ctx, ev)

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v err=%v, want 1", jobs, err)
	}
	p := jobs[0].Payload
	if _, ok := p["target_name"]; ok {
		t.Errorf("leaks target_name to untrusted channel: %+v", p)
	}
	if body, _ := p["body"].(string); strings.Contains(body, "secret-checkout") {
		t.Errorf("body leaks SLO name: %q", body)
	}
	if p["url"] == nil {
		t.Errorf("lost url after redaction: %+v", p)
	}
}
