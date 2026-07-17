package alert_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newEvalProject создаёт изолированный org+project с заданным slug (в
// отличие от newProject из alert_test.go, который хардкодит slug и годится
// только для одного вызова на пул). Подтесты Evaluator делят один пул, так
// что каждому нужен свой slug.
func newEvalProject(t *testing.T, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1, $1, 1000000) RETURNING id",
		slug).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1, $2, $2) RETURNING id",
		orgID, slug).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

// newEvalIssue вставляет issue напрямую — alert-пакет не зависит от issue.
func newEvalIssue(t *testing.T, pool *pgxpool.Pool, projectID int64, fingerprint string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO issues (project_id, fingerprint, title, culprit, level, first_seen, last_seen)
		VALUES ($1, $2, 'boom', 'app.x', 'error', now(), now()) RETURNING id`,
		projectID, fingerprint).Scan(&id); err != nil {
		t.Fatalf("issue: %v", err)
	}
	return id
}

func TestEvaluatorOnIssue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("enqueues one job per enabled channel with correct payload", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval1")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		ch1, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		})
		if err != nil {
			t.Fatalf("CreateChannel webhook: %v", err)
		}
		ch2, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
		})
		if err != nil {
			t.Fatalf("CreateChannel telegram: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
		e.OnIssue(ctx, alert.Event{
			ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue,
			Title: "boom", Culprit: "app.x", Level: "error", TimesSeen: 3,
		})

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

		wantURL := "https://gotcha.example/issues/" + strconv.FormatInt(issueID, 10)
		j1, ok := byChannel[ch1]
		if !ok {
			t.Fatalf("no job for webhook channel %d", ch1)
		}
		if j1.Payload["kind"] != alert.KindNewIssue ||
			j1.Payload["project_id"] != float64(pid) ||
			j1.Payload["issue_id"] != float64(issueID) ||
			j1.Payload["title"] != "boom" ||
			j1.Payload["culprit"] != "app.x" ||
			j1.Payload["level"] != "error" ||
			j1.Payload["times_seen"] != float64(3) ||
			j1.Payload["url"] != wantURL ||
			j1.Payload["channel_kind"] != alert.ChannelWebhook ||
			j1.Payload["target"] != "https://example.com/hook" {
			t.Errorf("webhook job payload = %+v, want url=%s", j1.Payload, wantURL)
		}
		if _, ok := j1.Payload["subject"].(string); !ok {
			t.Errorf("webhook job payload missing subject: %+v", j1.Payload)
		}
		if _, ok := j1.Payload["body"].(string); !ok {
			t.Errorf("webhook job payload missing body: %+v", j1.Payload)
		}

		j2, ok := byChannel[ch2]
		if !ok {
			t.Fatalf("no job for telegram channel %d", ch2)
		}
		if j2.Payload["channel_kind"] != alert.ChannelTelegram ||
			j2.Payload["target"] != "123" ||
			j2.Payload["secret"] != "tok" {
			t.Errorf("telegram job payload = %+v", j2.Payload)
		}
	})

	t.Run("throttle window suppresses repeat, shifting into the past re-fires", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval2")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
		ev := alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue, Title: "boom", Level: "error"}

		e.OnIssue(ctx, ev)
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("first call: jobs=%d err=%v, want 1", len(jobs), err)
		}
		for _, j := range jobs {
			if err := ob.MarkSent(ctx, j.ID); err != nil {
				t.Fatalf("MarkSent: %v", err)
			}
		}

		// Within throttle window: no new job.
		e.OnIssue(ctx, ev)
		jobs2, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs2) != 0 {
			t.Fatalf("throttled call: jobs=%d err=%v, want 0", len(jobs2), err)
		}

		// Push last_sent_at into the past -> throttle window elapsed -> fires again.
		if _, err := pool.Exec(ctx,
			"UPDATE alert_throttle SET last_sent_at = now() - interval '1 hour' WHERE issue_id = $1",
			issueID); err != nil {
			t.Fatalf("shift throttle: %v", err)
		}
		e.OnIssue(ctx, ev)
		jobs3, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs3) != 1 {
			t.Fatalf("after throttle shift: jobs=%d err=%v, want 1", len(jobs3), err)
		}
	})

	t.Run("disabled rule sends nothing", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval3")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: false, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue})
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("disabled rule: jobs=%d err=%v, want 0", len(jobs), err)
		}
	})

	t.Run("missing rule sends nothing", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval3b")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		// No rule upserted at all for KindRegression.
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindRegression})
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 0 {
			t.Fatalf("missing rule: jobs=%d err=%v, want 0", len(jobs), err)
		}
	})

	t.Run("disabled channel is skipped", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval4")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel disabled: %v", err)
		}
		enabledCh, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
		})
		if err != nil {
			t.Fatalf("CreateChannel enabled: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue})
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 || jobs[0].ChannelID != enabledCh {
			t.Fatalf("jobs = %+v err=%v, want exactly 1 job for channel %d", jobs, err, enabledCh)
		}
	})

	t.Run("email channel skipped when SMTP not configured", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval5")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com",
		}); err != nil {
			t.Fatalf("CreateChannel email: %v", err)
		}
		webhookCh, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		})
		if err != nil {
			t.Fatalf("CreateChannel webhook: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: false}
		e.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue})
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 || jobs[0].ChannelID != webhookCh {
			t.Fatalf("jobs = %+v err=%v, want exactly 1 job for webhook channel %d", jobs, err, webhookCh)
		}

		// With EmailEnabled=true on a fresh issue (avoids throttle interference), both channels fire.
		issueID2 := newEvalIssue(t, pool, pid, "fp-2")
		e2 := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: true}
		e2.OnIssue(ctx, alert.Event{ProjectID: pid, IssueID: issueID2, Kind: alert.KindNewIssue})
		jobs2, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs2) != 2 {
			t.Fatalf("jobs2 = %+v err=%v, want 2 (email now enabled)", jobs2, err)
		}
	})

	// ExternalDetails=false: во внешние каналы (Telegram/webhook) не должны
	// уезжать текст ошибки/детали (title/culprit/level/тело) — только ссылка
	// на issue и вид алерта. Защита от трансграничной передачи ПДн (152-ФЗ).
	t.Run("external details withheld from telegram/webhook when ExternalDetails=false", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval7")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		webhookCh, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		})
		if err != nil {
			t.Fatalf("CreateChannel webhook: %v", err)
		}
		telegramCh, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok",
		})
		if err != nil {
			t.Fatalf("CreateChannel telegram: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: false}
		e.OnIssue(ctx, alert.Event{
			ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue,
			Title: "boom", Culprit: "app.x", Level: "error", TimesSeen: 3,
		})

		jobs, err := ob.Claim(ctx, 10)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(jobs) != 2 {
			t.Fatalf("len(jobs) = %d, want 2", len(jobs))
		}

		wantURL := "https://gotcha.example/issues/" + strconv.FormatInt(issueID, 10)
		for _, j := range jobs {
			// Детали ошибки не должны попадать во внешний payload.
			if _, ok := j.Payload["title"]; ok {
				t.Errorf("channel %d: payload leaks title: %+v", j.ChannelID, j.Payload)
			}
			if _, ok := j.Payload["culprit"]; ok {
				t.Errorf("channel %d: payload leaks culprit: %+v", j.ChannelID, j.Payload)
			}
			if _, ok := j.Payload["level"]; ok {
				t.Errorf("channel %d: payload leaks level: %+v", j.ChannelID, j.Payload)
			}
			// Тело/subject не должны содержать текст ошибки.
			if body, _ := j.Payload["body"].(string); strings.Contains(body, "boom") || strings.Contains(body, "app.x") {
				t.Errorf("channel %d: body leaks error text: %q", j.ChannelID, body)
			}
			if subj, _ := j.Payload["subject"].(string); strings.Contains(subj, "boom") || strings.Contains(subj, "app.x") {
				t.Errorf("channel %d: subject leaks error text: %q", j.ChannelID, subj)
			}
			// Обезличенный минимум остаётся: ссылка и вид алерта.
			if j.Payload["url"] != wantURL {
				t.Errorf("channel %d: url = %v, want %s", j.ChannelID, j.Payload["url"], wantURL)
			}
			if j.Payload["kind"] != alert.KindNewIssue {
				t.Errorf("channel %d: kind = %v, want %s", j.ChannelID, j.Payload["kind"], alert.KindNewIssue)
			}
		}

		// Sanity: маршрутные поля на месте, чтобы worker собрал Target.
		byChannel := map[int64]notify.Job{}
		for _, j := range jobs {
			byChannel[j.ChannelID] = j
		}
		if byChannel[webhookCh].Payload["target"] != "https://example.com/hook" {
			t.Errorf("webhook target lost: %+v", byChannel[webhookCh].Payload)
		}
		if byChannel[telegramCh].Payload["secret"] != "tok" {
			t.Errorf("telegram secret lost: %+v", byChannel[telegramCh].Payload)
		}
	})

	// ExternalDetails=true: поведение прежнее — детали доставляются во внешние
	// каналы без изменений (обратная совместимость).
	t.Run("external details delivered to telegram/webhook when ExternalDetails=true", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval8")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel webhook: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example", ExternalDetails: true}
		e.OnIssue(ctx, alert.Event{
			ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue,
			Title: "boom", Culprit: "app.x", Level: "error", TimesSeen: 3,
		})

		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("Claim: jobs=%d err=%v, want 1", len(jobs), err)
		}
		if jobs[0].Payload["title"] != "boom" || jobs[0].Payload["culprit"] != "app.x" {
			t.Errorf("external details missing at ExternalDetails=true: %+v", jobs[0].Payload)
		}
	})

	// TestEvaluatorConcurrentOnIssueClaimsThrottleOnce covers the race
	// documented in issue.Upsert: two pipeline workers can both observe
	// New=true for the very first event of a fingerprint and both call
	// OnIssue concurrently for the same (issue, rule). The throttle claim
	// must serialize them so only one actually enqueues jobs, not N.
	t.Run("concurrent OnIssue for the same issue+rule claims the throttle exactly once", func(t *testing.T) {
		pid := newEvalProject(t, pool, "eval6")
		issueID := newEvalIssue(t, pool, pid, "fp-1")
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		if _, err := svc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		}); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}

		e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
		ev := alert.Event{ProjectID: pid, IssueID: issueID, Kind: alert.KindNewIssue, Title: "boom", Level: "error"}

		const n = 20
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				e.OnIssue(ctx, ev)
			}()
		}
		wg.Wait()

		jobs, err := ob.Claim(ctx, 100)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if len(jobs) != 1 {
			t.Fatalf("concurrent OnIssue: enqueued %d jobs, want exactly 1 (throttle must serialize)", len(jobs))
		}
	})
}
