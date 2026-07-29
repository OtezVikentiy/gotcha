package alert_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestAlertBudgetSuppressesFloodAndKeepsCount фиксирует дефект, из-за которого
// потолок понадобился: троттлинг alert_throttle ключуется парой
// (issue_id, rule_id), и у НОВОГО issue строки там нет по определению — он
// проходит всегда. Отправитель с уникальным fingerprint на каждое событие
// получал issue на событие и уведомление на событие, а ключ DSN публичен
// (он лежит в браузерном SDK).
func TestAlertBudgetSuppressesFloodAndKeepsCount(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetBudget(time.Hour, 3)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "budget")
	ch, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	ob := notify.NewOutbox(pool)

	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 0,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}

	// Десять РАЗНЫХ issue: пер-issue троттлинг их не сдерживает — у каждого
	// своя строка в alert_throttle, и каждая claim'ится успешно.
	for i := 0; i < 10; i++ {
		issueID := newEvalIssue(t, pool, pid, fmt.Sprintf("fp-budget-%d", i))
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue, Title: "boom"})
	}

	jobs, err := ob.Claim(ctx, 100)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("поставлено %d задач, want 3 (потолок) — бюджет не сработал", len(jobs))
	}
	for _, j := range jobs {
		if j.ChannelID != ch {
			t.Fatalf("задача ушла в канал %d, want %d", j.ChannelID, ch)
		}
	}
}

// TestAlertBudgetDigestReportsSuppressed — подавленное не теряется: после
// истечения окна уходит сводка с числом. Потолок без сводки — молчаливая
// потеря, а «тишина в Telegram» неотличима от «всё спокойно».
func TestAlertBudgetDigestReportsSuppressed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	// Крошечное окно, чтобы оно истекло к моменту сбора сводки.
	svc.SetBudget(time.Second, 2)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "digest")
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	ob := notify.NewOutbox(pool)
	e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}

	for i := 0; i < 7; i++ {
		issueID := newEvalIssue(t, pool, pid, fmt.Sprintf("fp-digest-%d", i))
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue, Title: "boom"})
	}
	if _, err := ob.Claim(ctx, 100); err != nil { // разгружаем очередь
		t.Fatalf("Claim: %v", err)
	}

	// Ждём истечения окна: сводка забирается ТОЛЬКО по его закрытию, иначе она
	// сообщила бы неполное число посреди всплеска.
	time.Sleep(1200 * time.Millisecond)

	batches, err := svc.ClaimSuppressed(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimSuppressed: %v", err)
	}
	var got int
	for _, b := range batches {
		if b.ProjectID == pid {
			got = b.Suppressed
		}
	}
	if got != 5 {
		t.Fatalf("подавленных в сводке %d, want 5 (7 событий − потолок 2)", got)
	}

	// Повторный забор ничего не даёт: счётчик обнулён в той же транзакции,
	// иначе две реплики разослали бы одну сводку дважды.
	again, err := svc.ClaimSuppressed(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimSuppressed повторно: %v", err)
	}
	for _, b := range again {
		if b.ProjectID == pid {
			t.Fatalf("повторный забор вернул %d подавленных — счётчик не обнулён", b.Suppressed)
		}
	}
}

// TestAlertBudgetDisabled — потолок 0 означает «выключено», а не «ничего не
// пропускать».
func TestAlertBudgetDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetBudget(time.Hour, 0)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "nobudget")
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	ob := notify.NewOutbox(pool)
	e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}

	for i := 0; i < 20; i++ {
		issueID := newEvalIssue(t, pool, pid, fmt.Sprintf("fp-nobudget-%d", i))
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue, Title: "boom"})
	}
	jobs, err := ob.Claim(ctx, 100)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 20 {
		t.Fatalf("поставлено %d задач, want 20 — выключенный потолок не должен ничего резать", len(jobs))
	}
}

// TestDigesterSendsSummary — рассыльщик сводок доводит подавленное до каналов.
// Потолок без сводки — молчаливая потеря: «тишина в Telegram» неотличима от
// «всё спокойно», а это ровно то, чего продукт мониторинга допускать не должен.
func TestDigesterSendsSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	svc.SetBudget(time.Second, 1)
	ctx := context.Background()

	pid := newEvalProject(t, pool, "digester")
	webhookCh, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Выключенный канал сводку получать не должен.
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off",
	}); err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}
	if _, err := svc.UpsertRule(ctx, alert.Rule{ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	ob := notify.NewOutbox(pool)
	e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
	for i := 0; i < 4; i++ {
		issueID := newEvalIssue(t, pool, pid, fmt.Sprintf("fp-dg-%d", i))
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue, Title: "boom"})
	}
	if _, err := ob.Claim(ctx, 100); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	time.Sleep(1200 * time.Millisecond) // окно должно закрыться

	d := &alert.Digester{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
	d.Tick(ctx)

	jobs, err := ob.Claim(ctx, 100)
	if err != nil {
		t.Fatalf("Claim digest: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("задач сводки %d, want 1 (только включённый канал)", len(jobs))
	}
	if jobs[0].ChannelID != webhookCh {
		t.Fatalf("сводка ушла в канал %d, want %d", jobs[0].ChannelID, webhookCh)
	}
	if jobs[0].Payload["kind"] != "suppressed_digest" {
		t.Errorf("вид задачи %v, want suppressed_digest", jobs[0].Payload["kind"])
	}
	if n, _ := jobs[0].Payload["count"].(float64); int(n) != 3 {
		t.Errorf("в сводке %v подавленных, want 3 (4 события − потолок 1)", jobs[0].Payload["count"])
	}

	// Повторный тик ничего не шлёт: счётчик обнулён при заборе, иначе сводка
	// уходила бы на каждом тике.
	d.Tick(ctx)
	again, err := ob.Claim(ctx, 100)
	if err != nil {
		t.Fatalf("Claim повторно: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("повторный тик отправил %d сводок — счётчик не обнулён", len(again))
	}
}

// TestDigesterRunStops — цикл рассыльщика завершается по отмене контекста и не
// падает на пустой работе.
func TestDigesterRunStops(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	d := &alert.Digester{Svc: svc, Outbox: notify.NewOutbox(pool), Interval: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run не завершился по отмене контекста")
	}
}
