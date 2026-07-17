package trace_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

func TestOutboxNotifierNotifyNewEnqueuesPerChannel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	psvc := trace.NewIssueService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "notif1")
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

	rec, err := psvc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	iss := rec.Issue

	n := &trace.OutboxNotifier{Alerts: asvc, Outbox: ob, Pool: pool, BaseURL: "https://gotcha.example", ExternalDetails: true}
	if err := n.NotifyNew(ctx, pid, iss); err != nil {
		t.Fatalf("NotifyNew: %v", err)
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

	wantURL := "https://gotcha.example/perf-issues/" + strconv.FormatInt(iss.ID, 10)
	wantSubject := "[Gotcha] Performance: " + iss.Title

	j1, ok := byChannel[webhookCh]
	if !ok {
		t.Fatalf("no job for webhook channel %d", webhookCh)
	}
	if j1.Payload["kind"] != trace.KindNPlusOne ||
		j1.Payload["project_id"] != float64(pid) ||
		j1.Payload["perf_issue_id"] != float64(iss.ID) ||
		j1.Payload["title"] != iss.Title ||
		j1.Payload["culprit"] != "GET /api/users" ||
		j1.Payload["count"] != float64(1) ||
		j1.Payload["url"] != wantURL ||
		j1.Payload["subject"] != wantSubject ||
		j1.Payload["channel_kind"] != alert.ChannelWebhook ||
		j1.Payload["target"] != "https://example.com/hook" {
		t.Errorf("webhook job payload = %+v", j1.Payload)
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
		j2.Payload["subject"] != wantSubject {
		t.Errorf("telegram job payload = %+v", j2.Payload)
	}
}

// Трансграничный гейт: при ExternalDetails=false во внешние каналы
// (Telegram/webhook) не должны уезжать iss.Title/iss.Culprit (имя транзакции,
// текст SQL — потенциальные ПДн, 152-ФЗ); при true — уезжают, как раньше.
func TestOutboxNotifierExternalDetailsGate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	psvc := trace.NewIssueService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("withheld when false", func(t *testing.T) {
		pid := newPerfProject(t, pool, "perf-ext-off")
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
		rec, err := psvc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
		if err != nil {
			t.Fatalf("Record: %v", err)
		}

		n := &trace.OutboxNotifier{Alerts: asvc, Outbox: ob, Pool: pool, BaseURL: "https://gotcha.example", ExternalDetails: false}
		if err := n.NotifyNew(ctx, pid, rec.Issue); err != nil {
			t.Fatalf("NotifyNew: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 2 {
			t.Fatalf("jobs = %+v err=%v, want 2", jobs, err)
		}
		byChannel := map[int64]notify.Job{}
		for _, j := range jobs {
			byChannel[j.ChannelID] = j
		}
		for _, id := range []int64{webhookCh, telegramCh} {
			p := byChannel[id].Payload
			if _, ok := p["title"]; ok {
				t.Errorf("channel %d leaks title: %+v", id, p)
			}
			if _, ok := p["culprit"]; ok {
				t.Errorf("channel %d leaks culprit: %+v", id, p)
			}
			if body, _ := p["body"].(string); strings.Contains(body, "GET /api/users") {
				t.Errorf("channel %d body leaks culprit: %q", id, body)
			}
			if p["url"] == nil {
				t.Errorf("channel %d lost url: %+v", id, p)
			}
		}
		if byChannel[telegramCh].Payload["secret"] != "tok" {
			t.Errorf("telegram secret lost: %+v", byChannel[telegramCh].Payload)
		}
	})

	t.Run("delivered when true", func(t *testing.T) {
		pid := newPerfProject(t, pool, "perf-ext-on")
		if _, err := asvc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel webhook: %v", err)
		}
		rec, err := psvc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		n := &trace.OutboxNotifier{Alerts: asvc, Outbox: ob, Pool: pool, BaseURL: "https://gotcha.example", ExternalDetails: true}
		if err := n.NotifyNew(ctx, pid, rec.Issue); err != nil {
			t.Fatalf("NotifyNew: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %+v err=%v, want 1", jobs, err)
		}
		if jobs[0].Payload["title"] != rec.Issue.Title || jobs[0].Payload["culprit"] != "GET /api/users" {
			t.Errorf("external details missing at ExternalDetails=true: %+v", jobs[0].Payload)
		}
	})
}

// Регрессия (проблему починили, она вернулась) алертит отдельным заголовком:
// дежурному важно отличить «нашли впервые» от «сломалось опять».
func TestOutboxNotifierNotifyRegression(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	psvc := trace.NewIssueService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "notif3")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	rec, err := psvc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	n := &trace.OutboxNotifier{Alerts: asvc, Outbox: ob, Pool: pool, BaseURL: "https://gotcha.example", ExternalDetails: true}
	if err := n.NotifyRegression(ctx, pid, rec.Issue); err != nil {
		t.Fatalf("NotifyRegression: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v err=%v, want 1", jobs, err)
	}
	if want := "[Gotcha] Performance regression: " + rec.Issue.Title; jobs[0].Payload["subject"] != want {
		t.Errorf("subject = %v, want %q", jobs[0].Payload["subject"], want)
	}
	if jobs[0].Payload["regression"] != true {
		t.Errorf("payload regression = %v, want true", jobs[0].Payload["regression"])
	}
}

func TestOutboxNotifierSkipsDisabledAndEmailChannels(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	psvc := trace.NewIssueService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "notif2")
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

	rec, err := psvc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	iss := rec.Issue

	n := &trace.OutboxNotifier{Alerts: asvc, Outbox: ob, Pool: pool, BaseURL: "https://gotcha.example", EmailEnabled: false, ExternalDetails: true}
	if err := n.NotifyNew(ctx, pid, iss); err != nil {
		t.Fatalf("NotifyNew: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 || jobs[0].ChannelID != enabledCh {
		t.Fatalf("jobs = %+v err=%v, want exactly 1 job for channel %d", jobs, err, enabledCh)
	}
}

// Слот часового лимита не должен сгорать впустую: если ни один Enqueue не
// прошёл (PG моргнула), разослано НИЧЕГО, и занятый слот означал бы, что бюджет
// проекта на час потрачен на несостоявшийся алерт.
func TestOutboxNotifierReleasesSlotWhenEnqueueFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	psvc := trace.NewIssueService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "enqueue-fail")
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Outbox на ЗАКРЫТОМ пуле: Enqueue гарантированно падает, клейм слота (n.Pool)
	// при этом работает.
	broken, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatalf("broken pool: %v", err)
	}
	broken.Close()

	rec, err := psvc.Record(ctx, pid, nPlusOneFinding(), "trace-a")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	n := &trace.OutboxNotifier{
		Alerts: asvc, Outbox: notify.NewOutbox(broken), Pool: pool, BaseURL: "https://gotcha.example",
	}
	if err := n.NotifyNew(ctx, pid, rec.Issue); err == nil {
		t.Fatal("NotifyNew: err = nil, want ошибку постановки в outbox")
	}

	var sent int
	if err := pool.QueryRow(ctx,
		"SELECT coalesce(sum(sent), 0) FROM perf_alert_throttle WHERE project_id = $1", pid).Scan(&sent); err != nil {
		t.Fatalf("perf_alert_throttle: %v", err)
	}
	if sent != 0 {
		t.Fatalf("sent = %d, want 0: слот часового лимита сгорел на несостоявшемся алерте", sent)
	}
}

// Троттлинг: у алертов о производительности не было НИ ОДНОГО ограничителя, и
// проект, у которого детекция нашла проблему на каждом эндпойнте, получал бы
// сообщение на каждую. Разрешено не больше maxPerfAlertsPerHour на проект в час;
// проблемы при этом продолжают ЗАПИСЫВАТЬСЯ, не рассылается только лишнее.
func TestOutboxNotifierThrottlesPerProject(t *testing.T) {
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	psvc := trace.NewIssueService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pid := newPerfProject(t, pool, "throttle")
	other := newPerfProject(t, pool, "throttle-other")
	for _, p := range []int64{pid, other} {
		if _, err := asvc.CreateChannel(ctx, alert.Channel{
			ProjectID: p, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
	}

	n := &trace.OutboxNotifier{Alerts: asvc, Outbox: ob, Pool: pool, BaseURL: "https://gotcha.example", ExternalDetails: true}

	// 30 РАЗНЫХ проблем одного проекта: рассылается ровно лимит.
	const attempts = 30
	for i := 0; i < attempts; i++ {
		f := nPlusOneFinding()
		f.Fingerprint = "fp-throttle-" + strconv.Itoa(i)
		rec, err := psvc.Record(ctx, pid, f, "trace-"+strconv.Itoa(i))
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if !rec.Created {
			t.Fatalf("проблема %d не создана", i)
		}
		if err := n.NotifyNew(ctx, pid, rec.Issue); err != nil {
			t.Fatalf("NotifyNew: %v", err)
		}
	}

	var perfIssues int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM perf_issues WHERE project_id = $1", pid).Scan(&perfIssues); err != nil {
		t.Fatalf("count perf_issues: %v", err)
	}
	if perfIssues != attempts {
		t.Errorf("perf_issues = %d, want %d: троттлинг ограничивает рассылку, а не запись",
			perfIssues, attempts)
	}

	jobs, err := ob.Claim(ctx, attempts+10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != trace.MaxPerfAlertsPerHour {
		t.Fatalf("задач в outbox = %d, want %d (по одному каналу на алерт)",
			len(jobs), trace.MaxPerfAlertsPerHour)
	}

	// Лимит у каждого проекта свой: выбранный лимит соседа не глушит.
	rec, err := psvc.Record(ctx, other, nPlusOneFinding(), "trace-other")
	if err != nil {
		t.Fatalf("Record other: %v", err)
	}
	if err := n.NotifyNew(ctx, other, rec.Issue); err != nil {
		t.Fatalf("NotifyNew other: %v", err)
	}
	jobs2, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim other: %v", err)
	}
	if len(jobs2) != 1 {
		t.Fatalf("задач соседнего проекта = %d, want 1", len(jobs2))
	}
}
