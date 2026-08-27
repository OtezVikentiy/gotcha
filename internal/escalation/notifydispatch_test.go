package escalation_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// dchanFrom converts a real alert.Channel into escalation.DispatchChannel the
// way every one of the seven callers (host/metric/slo/profile/trace/uptime/
// alert) does — this is the actual contract Dispatch is tested against, not a
// hand-rolled shortcut.
func dchanFrom(ch alert.Channel, emailEnabled bool, details alert.DetailPolicy) escalation.DispatchChannel {
	return escalation.DispatchChannel{
		ID: ch.ID, Kind: ch.Kind, Target: ch.Target,
		IsEmail:       ch.Kind == alert.ChannelEmail,
		Deliverable:   ch.Deliverable(),
		AllowsDetails: details.AllowsDetails(ch),
	}
}

// seedProject creates an organization+project with the given name directly
// against the test PG — the escalation package doesn't own project creation
// (org does, and org.Service.CreateProject validates the slug), so a raw
// insert keeps this test independent of org's own rules.
func seedProject(t *testing.T, pool *pgxpool.Pool, name string) int64 {
	t.Helper()
	ctx := context.Background()
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Dispatch',1000000) RETURNING id",
		"dispatch-org-"+name).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,$3) RETURNING id",
		orgID, "dispatch-proj-"+name, name).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func testDeps(ob *notify.Outbox) escalation.DispatchDeps {
	return escalation.DispatchDeps{Outbox: ob, EmailEnabled: true, LogTag: "test"}
}

// TestDispatchSkipsNonDeliverableChannel — гейт доставляемости: канал,
// который alert.Channel.Deliverable() считает недоставляемым (выключен),
// не получает задачу, даже если он в списке Channels.
func TestDispatchSkipsNonDeliverableChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "deliverable-gate")

	on, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/on"})
	if err != nil {
		t.Fatalf("create channel on: %v", err)
	}
	off, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off"})
	if err != nil {
		t.Fatalf("create channel off: %v", err)
	}
	details := alert.NewDetailPolicy("", nil, true)
	channels := []escalation.DispatchChannel{
		dchanFrom(alert.Channel{ID: on, Kind: alert.ChannelWebhook, Target: "https://example.com/on", Enabled: true}, true, details),
		dchanFrom(alert.Channel{ID: off, Kind: alert.ChannelWebhook, Target: "https://example.com/off", Enabled: false}, true, details),
	}

	enqueued, err := escalation.Dispatch(ctx, testDeps(ob), escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "s", Body: "b", URL: "https://x/y", Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0] != on {
		t.Fatalf("enqueued = %v, want [%d] (только включённый канал)", enqueued, on)
	}
	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ChannelID != on {
		t.Fatalf("jobs = %+v, want exactly 1 job for channel %d", jobs, on)
	}
}

// TestDispatchChannelIDsFiltersAfterDeliverable — ContainsID: непустой
// ChannelIDs сужает до перечисленных каналов ПОСЛЕ гейта доставляемости —
// выключенный канал не получает задачу, даже если он в ChannelIDs.
func TestDispatchChannelIDsFiltersAfterDeliverable(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "channelids-gate")

	c1, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c1"})
	c2, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/c2"})
	disabled, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off"})
	details := alert.NewDetailPolicy("", nil, true)
	channels := []escalation.DispatchChannel{
		dchanFrom(alert.Channel{ID: c1, Kind: alert.ChannelWebhook, Enabled: true}, true, details),
		dchanFrom(alert.Channel{ID: c2, Kind: alert.ChannelWebhook, Enabled: true}, true, details),
		dchanFrom(alert.Channel{ID: disabled, Kind: alert.ChannelWebhook, Enabled: false}, true, details),
	}

	enqueued, err := escalation.Dispatch(ctx, testDeps(ob), escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "s", Body: "b", URL: "https://x/y",
		ChannelIDs: []int64{c1, disabled}, Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0] != c1 {
		t.Fatalf("enqueued = %v, want [%d] (c1 в наборе и доставляем; disabled в наборе, но не доставляем; c2 доставляем, но не в наборе)", enqueued, c1)
	}
}

// TestDispatchEmailFallbackSkipsEmailWhenDisabled — email-fallback: канал
// kind=email пропускается при EmailEnabled=false, остальные виды — нет.
func TestDispatchEmailFallbackSkipsEmailWhenDisabled(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "email-fallback")

	email, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelEmail, Enabled: true, Target: "ops@example.com"})
	webhook, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	details := alert.NewDetailPolicy("", nil, true)
	channels := []escalation.DispatchChannel{
		dchanFrom(alert.Channel{ID: email, Kind: alert.ChannelEmail, Enabled: true}, true, details),
		dchanFrom(alert.Channel{ID: webhook, Kind: alert.ChannelWebhook, Enabled: true}, true, details),
	}

	deps := testDeps(ob)
	deps.EmailEnabled = false
	enqueued, err := escalation.Dispatch(ctx, deps, escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "s", Body: "b", URL: "https://x/y", Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0] != webhook {
		t.Fatalf("enqueued = %v, want [%d] (email пропущен, SMTP не настроен)", enqueued, webhook)
	}
}

// stubProjectNamer — фиксированное имя проекта, без обращения к БД.
type stubProjectNamer struct {
	name string
	err  error
}

func (s stubProjectNamer) ProjectName(context.Context, int64) (string, error) {
	return s.name, s.err
}

// TestDispatchProjectNameInSubjectBodyAndPayload — W3-E требование 4: имя
// проекта попадает в тему, тело и payload (webhook), когда Projects задан.
func TestDispatchProjectNameInSubjectBodyAndPayload(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	pid := seedProject(t, pool, "with-name")

	ch, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	details := alert.NewDetailPolicy("", nil, true)
	channels := []escalation.DispatchChannel{dchanFrom(alert.Channel{ID: ch, Kind: alert.ChannelWebhook, Enabled: true}, true, details)}

	deps := testDeps(ob)
	deps.Projects = stubProjectNamer{name: "Marketing Site"}
	_, err := escalation.Dispatch(ctx, deps, escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "[Gotcha] alert fired", Body: "details here", URL: "https://x/y", Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("Claim: jobs=%+v err=%v", jobs, err)
	}
	subject, _ := jobs[0].Payload["subject"].(string)
	body, _ := jobs[0].Payload["body"].(string)
	if subject != "[Gotcha] alert fired · Marketing Site" {
		t.Errorf("subject = %q, want project name appended", subject)
	}
	if body != "Проект: Marketing Site\n\ndetails here" {
		t.Errorf("body = %q, want project name line prepended", body)
	}
	if jobs[0].Payload["project_name"] != "Marketing Site" {
		t.Errorf("payload project_name = %v, want %q", jobs[0].Payload["project_name"], "Marketing Site")
	}
}

// TestDispatchNoProjectsFieldOmitsProjectName — nil-совместимость: без
// Projects поведение в точности как до W3-E — ни в subject/body, ни в
// payload имени проекта нет.
func TestDispatchNoProjectsFieldOmitsProjectName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "no-namer")

	ch, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	details := alert.NewDetailPolicy("", nil, true)
	channels := []escalation.DispatchChannel{dchanFrom(alert.Channel{ID: ch, Kind: alert.ChannelWebhook, Enabled: true}, true, details)}

	_, err := escalation.Dispatch(ctx, testDeps(ob), escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "plain subject", Body: "plain body", URL: "https://x/y", Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1", jobs)
	}
	if jobs[0].Payload["subject"] != "plain subject" || jobs[0].Payload["body"] != "plain body" {
		t.Errorf("subject/body must be untouched without Projects: %+v", jobs[0].Payload)
	}
	if _, ok := jobs[0].Payload["project_name"]; ok {
		t.Errorf("project_name must be absent without Projects, got %v", jobs[0].Payload["project_name"])
	}
}

// TestDispatchDegradesToNoProjectNameOnResolverError — resolveProjectName
// (найдено ревью): когда Projects.ProjectName возвращает ошибку (проект
// успел исчезнуть между событием и доставкой, сбой БД резолвера и т.п.),
// Dispatch не должен ронять уведомление целиком — деградирует до "" (то же
// поведение, что и при nil Projects), молча (для получателя) отбрасывая
// только имя проекта, не всё уведомление.
func TestDispatchDegradesToNoProjectNameOnResolverError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "resolver-error")

	ch, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	details := alert.NewDetailPolicy("", nil, true)
	channels := []escalation.DispatchChannel{dchanFrom(alert.Channel{ID: ch, Kind: alert.ChannelWebhook, Enabled: true}, true, details)}

	deps := testDeps(ob)
	// name задан НАРЯДУ с err: реальный ProjectNamer тоже может отдать
	// частичный результат вместе с ошибкой (см. ProjectName у
	// escalation.OrgProjectNamer — org.GetProject возвращает пустой Project
	// при ошибке, но контракт интерфейса err первичен). Если бы Dispatch
	// читал name, не проверив err, "should-not-appear" утекло бы в subject/
	// body/payload — этого достаточно, чтобы отличить пустое имя ПОТОМУ ЧТО
	// резолвер ошибся от пустого имени ПОТОМУ ЧТО оно и должно быть пустым.
	deps.Projects = stubProjectNamer{name: "should-not-appear", err: errors.New("project lookup boom")}
	enqueued, err := escalation.Dispatch(ctx, deps, escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "plain subject", Body: "plain body", URL: "https://x/y", Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v (resolver error must not fail the whole dispatch)", err)
	}
	if len(enqueued) != 1 {
		t.Fatalf("enqueued = %v, want 1 channel (resolver error must not block delivery)", enqueued)
	}
	jobs, jerr := ob.Claim(ctx, 10)
	if jerr != nil || len(jobs) != 1 {
		t.Fatalf("Claim: jobs=%+v err=%v", jobs, jerr)
	}
	if jobs[0].Payload["subject"] != "plain subject" || jobs[0].Payload["body"] != "plain body" {
		t.Errorf("subject/body must stay untouched when ProjectName errors: %+v", jobs[0].Payload)
	}
	if _, ok := jobs[0].Payload["project_name"]; ok {
		t.Errorf("project_name must be absent when ProjectName errors, got %v", jobs[0].Payload["project_name"])
	}
}

// TestDispatchRedactsExternalChannelButKeepsProjectName — редакция ПДн:
// канал без AllowsDetails получает обезличенный payload (доменные Extra-поля
// вырезаны), но project_name переживает редакцию (W3-E: обезличенный путь —
// как раз тот случай, где имя проекта нужнее всего, единственный
// опознаватель внешнего канала на несколько проектов).
func TestDispatchRedactsExternalChannelButKeepsProjectName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	pid := seedProject(t, pool, "redacted")

	ch, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "123", Secret: "tok"})
	channels := []escalation.DispatchChannel{{ID: ch, Kind: alert.ChannelTelegram, Target: "123", Deliverable: true, AllowsDetails: false}}

	deps := testDeps(ob)
	deps.Projects = stubProjectNamer{name: "Secret Corp"}
	_, err := escalation.Dispatch(ctx, deps, escalation.DispatchInput{
		ProjectID: pid, Kind: "host_alert_open", Subject: "leaky subject with hostname db-07", Body: "leaky body with IP 10.0.0.5",
		URL: "https://gotcha.example/projects/1/hosts/db-07", Extra: map[string]any{"host_name": "db-07", "detail": "disk 95%"},
		Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1", jobs)
	}
	p := jobs[0].Payload
	if _, ok := p["host_name"]; ok {
		t.Errorf("redacted payload must not carry domain extra fields, got host_name=%v", p["host_name"])
	}
	if _, ok := p["detail"]; ok {
		t.Errorf("redacted payload must not carry domain extra fields, got detail=%v", p["detail"])
	}
	if p["project_name"] != "Secret Corp" {
		t.Errorf("project_name must survive redaction, got %v", p["project_name"])
	}
	subject, _ := p["subject"].(string)
	body, _ := p["body"].(string)
	if subject == "" || body == "" {
		t.Fatalf("redacted subject/body must not be empty: subject=%q body=%q", subject, body)
	}
	if subject == "leaky subject with hostname db-07" {
		t.Errorf("subject must be replaced by the redacted template, got original: %q", subject)
	}
	// Проект по-прежнему называется в обезличенном тексте.
	if !strings.Contains(subject, "Secret Corp") || !strings.Contains(body, "Secret Corp") {
		t.Errorf("redacted subject/body must still name the project: subject=%q body=%q", subject, body)
	}
}

// TestDispatchRedactedURLOverridesURL — url_redacted (host): канал без
// AllowsDetails получает URL из RedactedURL, а не полный URL.
func TestDispatchRedactedURLOverridesURL(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "redacted-url")

	ch, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true, Target: "1", Secret: "tok"})
	channels := []escalation.DispatchChannel{{ID: ch, Kind: alert.ChannelTelegram, Target: "1", Deliverable: true, AllowsDetails: false}}

	_, err := escalation.Dispatch(ctx, testDeps(ob), escalation.DispatchInput{
		ProjectID: pid, Kind: "host_alert_open", Subject: "s", Body: "b",
		URL: "https://gotcha.example/projects/1/hosts/db-07", RedactedURL: "https://gotcha.example/projects/1/hosts",
		Channels: channels,
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1", jobs)
	}
	if jobs[0].Payload["url"] != "https://gotcha.example/projects/1/hosts" {
		t.Errorf("url = %v, want RedactedURL (карточка хоста несёт имя машины)", jobs[0].Payload["url"])
	}
}

// TestDispatchPartialEnqueueFailureIsAggregatedAndDoesNotBlockOthers —
// негативный сценарий: канал с несуществующим ID (симулирует сбой Enqueue —
// FK на alert_channels) не мешает постановке для валидного канала, и ошибка
// возвращается агрегированной (errors.Join).
func TestDispatchPartialEnqueueFailureIsAggregatedAndDoesNotBlockOthers(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	pid := seedProject(t, pool, "partial-fail")

	good, _ := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	const bogus = int64(999_999_999)
	channels := []escalation.DispatchChannel{
		{ID: bogus, Kind: alert.ChannelWebhook, Target: "https://example.com/bogus", Deliverable: true, AllowsDetails: true},
		{ID: good, Kind: alert.ChannelWebhook, Target: "https://example.com/hook", Deliverable: true, AllowsDetails: true},
	}

	enqueued, err := escalation.Dispatch(ctx, testDeps(ob), escalation.DispatchInput{
		ProjectID: pid, Kind: "test_kind", Subject: "s", Body: "b", URL: "https://x/y", Channels: channels,
	})
	if err == nil {
		t.Fatalf("Dispatch: want an error for the bogus channel, got nil")
	}
	if !strings.Contains(err.Error(), "enqueue channel "+strconv.FormatInt(bogus, 10)) {
		t.Fatalf("error must name the failing channel %d, got: %v", bogus, err)
	}
	if len(enqueued) != 1 || enqueued[0] != good {
		t.Fatalf("enqueued = %v, want [%d] (только рабочий канал, несмотря на ошибку по битому)", enqueued, good)
	}
}

// Compile-time check: OrgProjectNamer must satisfy ProjectNamer.
var _ escalation.ProjectNamer = escalation.OrgProjectNamer{}

// TestOrgProjectNamerResolvesRealProjectName — интеграционная проверка
// адаптера поверх настоящего org.Service.GetProject.
func TestOrgProjectNamerResolvesRealProjectName(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool, "Adapter Target")
	namer := escalation.OrgProjectNamer{Svc: org.NewService(pool, 0)}

	name, err := namer.ProjectName(ctx, pid)
	if err != nil {
		t.Fatalf("ProjectName: %v", err)
	}
	if name != "Adapter Target" {
		t.Errorf("name = %q, want %q", name, "Adapter Target")
	}
}

// TestOrgProjectNamerNilServiceIsSilent — nil Svc (тесты, не заинтересованные
// в имени проекта) — не паникует, отдаёт "".
func TestOrgProjectNamerNilServiceIsSilent(t *testing.T) {
	namer := escalation.OrgProjectNamer{}
	name, err := namer.ProjectName(context.Background(), 1)
	if err != nil || name != "" {
		t.Errorf("nil Svc: got name=%q err=%v, want \"\", nil", name, err)
	}
}
