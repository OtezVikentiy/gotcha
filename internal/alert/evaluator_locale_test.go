package alert_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// stubEvalProjectNamer — фиксированное имя проекта, без обращения к БД.
type stubEvalProjectNamer struct{ name string }

func (s stubEvalProjectNamer) ProjectName(context.Context, int64) (string, error) {
	return s.name, nil
}

// TestEvaluatorDispatchUsesConfiguredLocale — реролл ревью W3-E: OnIssue
// строит subject/body на lctx := i18n.WithLocale(ctx, e.Locale) (класс
// №133–136), но исходно звал escalation.Dispatch(ctx, ...) — БАЗОВЫМ ctx, не
// lctx. Dispatch сам зовёт notify.WithProjectSubject/WithProjectBody и, на
// обезличенном пути, notify.RedactExternalPayload — обе берут локаль ИЗ ctx,
// которым их позвали, и без lctx откатывались бы на i18n.Default ("ru")
// независимо от GOTCHA_LOCALE, расходясь с уже локализованным (en) текстом
// самого OnIssue. У остальных шести источников (host/metric/slo/profile/
// trace/uptime) локаль в ctx для Dispatch кладётся всегда — только alert
// был исключением, и это прожило бы незамеченным: до этого теста локаль в
// evaluator_test.go не упоминалась вовсе.
//
// Канал telegram (AllowsDetails=false) бьёт РЕДАКТИРОВАННЫЙ путь: подпись
// вида алерта ("New issue" vs "Новая проблема") и обёртка "Project:"/
// "Проект:" различаются по языку сильнее всего — если бы Dispatch получил
// базовый ctx, обе ушли бы на русском при en-инстансе.
func TestEvaluatorDispatchUsesConfiguredLocale(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newEvalProject(t, pool, "eval-locale")
	issueID := newEvalIssue(t, pool, pid, "fp-locale")
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		ProjectID: pid, Kind: alert.KindNewIssue, Enabled: true, ThrottleMinutes: 30,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}
	webhookCh, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://internal.example/hook",
		// Trusted: этот канал должен бить НЕобезличенный путь (AllowsDetails
		// true) — telegram ниже бьёт обезличенный (DetailPolicy держит его
		// внешним всегда), так тест кроет ОБА места, где Dispatch зовёт
		// notify.WithProjectSubject/WithProjectBody: напрямую и через
		// RedactExternalPayload.
		Trusted: true,
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

	e := &alert.Evaluator{
		Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example",
		// Details: allowAll=false, trusted пуст — webhookCh получает полный
		// (не обезличенный) путь только через свой собственный Trusted=true;
		// telegramCh остаётся внешним всегда (DetailPolicy: chat_id не
		// разобрать как получателя) и бьёт RedactExternalPayload.
		Details:  alert.NewDetailPolicy("", nil, false),
		Locale:   i18n.Locale{Code: "en"},
		Projects: stubEvalProjectNamer{name: "Marketing Site"},
	}
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

	webhookJob, ok := byChannel[webhookCh]
	if !ok {
		t.Fatalf("no job for webhook channel %d", webhookCh)
	}
	webhookBody, _ := webhookJob.Payload["body"].(string)
	if !strings.Contains(webhookBody, "boom") || !strings.Contains(webhookBody, "app.x") {
		t.Fatalf("webhookCh is Trusted=true, body must be the FULL (non-redacted) issue body: %q", webhookBody)
	}
	if !strings.Contains(webhookBody, "Project: Marketing Site") {
		t.Errorf("webhook body must use the EN project wrapper (\"Project: ...\"), got: %q", webhookBody)
	}
	if strings.Contains(webhookBody, "Проект:") {
		t.Errorf("webhook body leaked the RU project wrapper despite Locale=en: %q", webhookBody)
	}

	telegramJob, ok := byChannel[telegramCh]
	if !ok {
		t.Fatalf("no job for telegram channel %d", telegramCh)
	}
	telegramSubject, _ := telegramJob.Payload["subject"].(string)
	telegramBody, _ := telegramJob.Payload["body"].(string)
	if !strings.Contains(telegramSubject, "New issue") {
		t.Errorf("redacted telegram subject must carry the EN kind label (\"New issue\"), got: %q", telegramSubject)
	}
	if strings.Contains(telegramSubject, "Новая проблема") {
		t.Errorf("redacted telegram subject leaked the RU kind label despite Locale=en: %q", telegramSubject)
	}
	if !strings.Contains(telegramBody, "Project: Marketing Site") {
		t.Errorf("redacted telegram body must use the EN project wrapper, got: %q", telegramBody)
	}
	if strings.Contains(telegramBody, "Проект:") {
		t.Errorf("redacted telegram body leaked the RU project wrapper despite Locale=en: %q", telegramBody)
	}
	if telegramJob.Payload["project_name"] != "Marketing Site" {
		t.Errorf("redacted telegram payload project_name = %v, want %q", telegramJob.Payload["project_name"], "Marketing Site")
	}
}

// compile-time sanity: stubEvalProjectNamer must satisfy escalation.ProjectNamer.
var _ escalation.ProjectNamer = stubEvalProjectNamer{}
