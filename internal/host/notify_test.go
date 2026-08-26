package host_test

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// seedNotifyChannel заводит один включённый webhook-канал проекта — этого
// достаточно, чтобы проверить постановку задачи в Outbox (сам webhook.go
// здесь не участвует).
func seedNotifyChannel(t *testing.T, asvc *alert.Service, projectID int64) {
	t.Helper()
	if _, err := asvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("create channel: %v", err)
	}
}

// TestHostNotifyRussianTextsQuoteEveryKind — регрессия UX-аудита A1 (P1-2).
// Русские шаблоны уведомлений подставляют вид порога внутрь кавычек-ёлочек и
// НЕ делают его подлежащим фразы: у видов из host.Kinds род разный («Память»,
// «Диск», «Нагрузка», «Тишина»), и любое сказуемое рядом с {kind} обязано
// разъехаться хотя бы с половиной из них — так и было («Память — вернулся»,
// «Тишина — вернулся»). Проверка идёт по КАТАЛОГУ на всех видах сразу, а не
// на одном виде через Outbox: postgres тут не нужен, а дефект по построению
// виден только на полном множестве Kinds.
func TestHostNotifyRussianTextsQuoteEveryKind(t *testing.T) {
	ctx := i18n.WithLocale(context.Background(), i18n.Locale{Code: "ru"})
	keys := []string{
		"notify.host_alert_open.subject", "notify.host_alert_open.body",
		"notify.host_alert_resolved.subject", "notify.host_alert_resolved.body",
	}
	for _, kind := range host.Kinds {
		label := i18n.T(ctx, "hosts.kind."+kind)
		for _, key := range keys {
			got := i18n.Tf(ctx, key, "host", "web-01", "kind", label,
				"value", "95.0%", "threshold_line", "", "detail_line", "",
				"deps_line", "", "url", "https://gotcha.example/x")
			if !strings.Contains(got, "«"+label+"»") {
				t.Errorf("[%s] вид %q подставлен без кавычек-ёлочек: %q", key, kind, got)
			}
			if strings.Contains(got, label+" —") {
				t.Errorf("[%s] вид %q стал подлежащим фразы — род сказуемого разъедется: %q", key, kind, got)
			}
		}
	}
}

func TestHostNotifierOpenedEnqueuesPerChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "web-01")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
	in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.953, PeakValue: 0.97, Detail: "/data"}
	s := host.Settings{DiskThreshold: 0.90}

	if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
		t.Fatalf("HostIncidentOpened: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Payload["kind"] != "host_alert_open" {
		t.Fatalf("kind = %v, want host_alert_open", jobs[0].Payload["kind"])
	}

	wantURL := fmt.Sprintf("https://gotcha.example/projects/%d/hosts/%s", projectID, url.PathEscape("web-01"))
	if jobs[0].Payload["url"] != wantURL {
		t.Errorf("url = %v, want %v", jobs[0].Payload["url"], wantURL)
	}

	body, _ := jobs[0].Payload["body"].(string)
	if !strings.Contains(body, "web-01") {
		t.Errorf("body missing host name: %q", body)
	}
	if !strings.Contains(body, "95.3%") {
		t.Errorf("body missing current value (95.3%%): %q", body)
	}
	if !strings.Contains(body, "90.0%") {
		t.Errorf("body missing threshold (90.0%%): %q", body)
	}
	if !strings.Contains(body, "/data") {
		t.Errorf("body missing mountpoint detail: %q", body)
	}
	if !strings.Contains(body, wantURL) {
		t.Errorf("body missing url: %q", body)
	}
	if jobs[0].Payload["channel_kind"] != alert.ChannelWebhook {
		t.Errorf("channel_kind = %v", jobs[0].Payload["channel_kind"])
	}
}

func TestHostNotifierLoadAndSilentUnits(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()

	t.Run("load — множитель", func(t *testing.T) {
		projectID := seedEvalProject(t, pool)
		h := seedEvalHost(t, pool, projectID, "load-host")
		seedNotifyChannel(t, asvc, projectID)
		n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
		in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "load", CurrentValue: 2.5, PeakValue: 3.1}
		s := host.Settings{LoadThreshold: 2.0}
		if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
			t.Fatalf("HostIncidentOpened: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
		}
		body, _ := jobs[0].Payload["body"].(string)
		if !strings.Contains(body, "2.50×") {
			t.Errorf("body missing load multiplier (2.50×): %q", body)
		}
	})

	t.Run("silent — длительность", func(t *testing.T) {
		projectID := seedEvalProject(t, pool)
		h := seedEvalHost(t, pool, projectID, "silent-host")
		seedNotifyChannel(t, asvc, projectID)
		n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
		in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "silent", CurrentValue: 600, PeakValue: 600}
		s := host.Settings{SilentAfter: 5 * time.Minute}
		if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
			t.Fatalf("HostIncidentOpened: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
		}
		body, _ := jobs[0].Payload["body"].(string)
		if !strings.Contains(body, "10") { // 600с = 10 минут
			t.Errorf("body missing silent duration (10 минут): %q", body)
		}
	})
}

func TestHostNotifierEnglishLocale(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "en-host")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{
		Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
		Details: alert.NewDetailPolicy("", nil, true),
		Locale:  i18n.Locale{Code: "en"},
	}
	in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "memory", CurrentValue: 0.92}
	s := host.Settings{MemoryThreshold: 0.90}
	if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
		t.Fatalf("HostIncidentOpened: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
	}
	body, _ := jobs[0].Payload["body"].(string)
	subject, _ := jobs[0].Payload["subject"].(string)
	if !strings.Contains(body, "Memory") {
		t.Errorf("body not in English (expected 'Memory'): %q", body)
	}
	if strings.Contains(body, "Хост") || strings.Contains(subject, "Хост") {
		t.Errorf("expected English text, got Russian: subject=%q body=%q", subject, body)
	}
}

// Трансграничный гейт: при политике без доверия получателю во внешние каналы
// не должно уезжать имя хоста/значения (тело/subject); при true — уезжает.
func TestHostNotifierExternalDetailsGate(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()

	t.Run("withheld when false", func(t *testing.T) {
		projectID := seedEvalProject(t, pool)
		h := seedEvalHost(t, pool, projectID, "gate-host")
		seedNotifyChannel(t, asvc, projectID)
		n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, false)}
		in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.95, Detail: "/data"}
		s := host.Settings{DiskThreshold: 0.90}
		if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
			t.Fatalf("HostIncidentOpened: %v", err)
		}

		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
		}
		p := jobs[0].Payload
		if _, ok := p["host_name"]; ok {
			t.Errorf("leaks host name: %+v", p)
		}
		if body, _ := p["body"].(string); strings.Contains(body, "/data") || strings.Contains(body, "95.0%") {
			t.Errorf("body leaks host details (value/mountpoint): %q", body)
		}
		if subj, _ := p["subject"].(string); strings.Contains(subj, "gate-host") {
			t.Errorf("subject leaks host name: %q", subj)
		}
		// Решение владельца на приёмке A1: имя хоста не уходит наружу и внутри
		// ссылки. Карточка адресуется именем (id-адресации у хоста нет),
		// поэтому обезличенный payload несёт ссылку на СПИСОК хостов — она
		// ведёт куда надо и имени не содержит.
		wantURL := fmt.Sprintf("https://gotcha.example/projects/%d/hosts", projectID)
		if p["url"] != wantURL {
			t.Errorf("url = %v, want список хостов %v", p["url"], wantURL)
		}
		if body, _ := p["body"].(string); strings.Contains(body, "gate-host") {
			t.Errorf("body leaks host name (внутри ссылки): %q", body)
		}
		// Директива редакции не должна доезжать до получателя отдельным полем:
		// webhook сериализует payload целиком.
		if _, ok := p["url_redacted"]; ok {
			t.Errorf("url_redacted утёк в доставляемый payload: %+v", p)
		}
	})

	t.Run("delivered when true", func(t *testing.T) {
		projectID := seedEvalProject(t, pool)
		h := seedEvalHost(t, pool, projectID, "gate-host-2")
		seedNotifyChannel(t, asvc, projectID)
		n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
		in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.95, Detail: "/data"}
		s := host.Settings{DiskThreshold: 0.90}
		if err := n.HostIncidentOpened(ctx, in, h, s); err != nil {
			t.Fatalf("HostIncidentOpened: %v", err)
		}

		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
		}
		if jobs[0].Payload["host_name"] != "gate-host-2" {
			t.Errorf("host_name missing with an allowing policy: %+v", jobs[0].Payload)
		}
		// Внутри контура ссылка — полная, на карточку хоста.
		wantURL := fmt.Sprintf("https://gotcha.example/projects/%d/hosts/%s", projectID, url.PathEscape("gate-host-2"))
		if jobs[0].Payload["url"] != wantURL {
			t.Errorf("url = %v, want карточку %v", jobs[0].Payload["url"], wantURL)
		}
		if _, ok := jobs[0].Payload["url_redacted"]; ok {
			t.Errorf("url_redacted попал в полный payload: %+v", jobs[0].Payload)
		}
	})
}

// TestHostNotifierRetiredEnqueuesOwnKind — снятие с наблюдения приходит своим
// видом и своим текстом: «вернулся в норму» про окончательно ушедшую машину
// сказать нельзя, оператор прочитал бы это как «сервер ожил». Ссылка ведёт на
// список хостов — карточки этого хоста сразу после прохода уже не будет.
func TestHostNotifierRetiredEnqueuesOwnKind(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "retired-host")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
	open := []host.Incident{
		{ProjectID: projectID, HostID: h.ID, Kind: "silent"},
		{ProjectID: projectID, HostID: h.ID, Kind: "disk"},
	}
	if err := n.HostRetired(ctx, h, open); err != nil {
		t.Fatalf("HostRetired: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d err=%v, want 1 (одно сообщение на хост, а не на инцидент)", len(jobs), err)
	}
	p := jobs[0].Payload
	if p["kind"] != "host_retired" {
		t.Fatalf("kind = %v, want host_retired", p["kind"])
	}
	if p["url"] != fmt.Sprintf("https://gotcha.example/projects/%d/hosts", projectID) {
		t.Errorf("url = %v, want список хостов (карточки уже не будет)", p["url"])
	}
	body, _ := p["body"].(string)
	if !strings.Contains(body, "retired-host") {
		t.Errorf("body без имени хоста: %q", body)
	}
	for _, want := range []string{"Тишина", "Диск"} {
		if !strings.Contains(body, want) {
			t.Errorf("body без вида закрытого порога %q: %q", want, body)
		}
	}
	if strings.Contains(body, "{") {
		t.Errorf("в тексте остался неподставленный плейсхолдер: %q", body)
	}
}

// TestHostNotifierRetiredWithheldExternally — снятие подчиняется тому же
// гейту трансграничной передачи, что и остальные уведомления хоста.
func TestHostNotifierRetiredWithheldExternally(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "retired-secret")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, false)}
	if err := n.HostRetired(ctx, h, []host.Incident{{ProjectID: projectID, HostID: h.ID, Kind: "silent"}}); err != nil {
		t.Fatalf("HostRetired: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %d err=%v, want 1", len(jobs), err)
	}
	p := jobs[0].Payload
	if _, ok := p["host_name"]; ok {
		t.Errorf("leaks host name: %+v", p)
	}
	if _, ok := p["host_kinds"]; ok {
		t.Errorf("leaks closed thresholds: %+v", p)
	}
	if body, _ := p["body"].(string); strings.Contains(body, "retired-secret") {
		t.Errorf("body leaks host name: %q", body)
	}
}

func TestHostNotifierResolvedEnqueuesKind(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "resolved-host")
	seedNotifyChannel(t, asvc, projectID)

	n := &host.HostNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true)}
	in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.80}
	if err := n.HostIncidentResolved(ctx, in, h); err != nil {
		t.Fatalf("HostIncidentResolved: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Payload["kind"] != "host_alert_resolved" {
		t.Fatalf("kind = %v, want host_alert_resolved", jobs[0].Payload["kind"])
	}
	body, _ := jobs[0].Payload["body"].(string)
	if !strings.Contains(body, "resolved-host") {
		t.Errorf("body missing host name: %q", body)
	}
	if !strings.Contains(body, "80.0%") {
		t.Errorf("body missing current value (80.0%%): %q", body)
	}
}

// TestHostNotifierReturnsEnqueueError — провал постановки в Outbox всплывает
// вызывающему, а не остаётся строкой в журнале. По этой ошибке Evaluator и
// решает не ставить notified_open (см.
// TestEvaluatorKeepsNotifiedFalseWhenNotifierFails): без неё «уведомлён»
// означало бы «нотификатора позвали», а не «в очередь встало».
//
// Отказ моделируется закрытым пулом ИМЕННО у Outbox: у Alerts пул живой,
// поэтому список каналов читается успешно и падает ровно Enqueue — то есть
// проверяется нужная ветка, а не ранний выход по ошибке чтения каналов.
func TestHostNotifierReturnsEnqueueError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	asvc := alert.NewService(pool)
	ctx := context.Background()
	projectID := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, projectID, "enqueue-fails")
	seedNotifyChannel(t, asvc, projectID)

	dead := testenv.MigratedPG(t)
	dead.Close()

	n := &host.HostNotifier{
		Alerts: asvc, Outbox: notify.NewOutbox(dead),
		BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true),
	}
	in := host.Incident{ProjectID: projectID, HostID: h.ID, Kind: "disk", CurrentValue: 0.95}
	s := host.Settings{DiskThreshold: 0.90}

	if err := n.HostIncidentOpened(ctx, in, h, s); err == nil {
		t.Error("HostIncidentOpened вернул nil при провале Enqueue — оператор увидит «уведомлён» без письма")
	}
	if err := n.HostIncidentResolved(ctx, in, h); err == nil {
		t.Error("HostIncidentResolved вернул nil при провале Enqueue")
	}
}
