package uptime_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
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

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
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
		j2.Payload["subject"] != wantSubject {
		t.Errorf("telegram job payload = %+v", j2.Payload)
	}
	if _, ok := j2.Payload["secret"]; ok {
		t.Errorf("секрет попал в payload очереди: %+v", j2.Payload)
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

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
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

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", EmailEnabled: false, Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
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

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
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
	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}

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

	// MonitorChannelIDs отдаёт СОБСТВЕННЫЕ каналы монитора, включая выключенные:
	// пустой результат означает «своих каналов нет» и включает фолбэк на каналы
	// проекта. Если бы выключенные отсеивались здесь, монитор с единственным
	// выключенным каналом выглядел бы как «без своих каналов» и его уведомления
	// уехали бы ВО ВСЕ каналы проекта — ровно в те, что оператор исключил.
	// Выключенные пропускает сам Notify (см. TestOutboxNotifierAllOwnChannelsDisabled).
	ids, err := usvc.MonitorChannelIDs(ctx, m.ID)
	if err != nil {
		t.Fatalf("MonitorChannelIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("MonitorChannelIDs = %v, want both linked channels (enabled and disabled)", ids)
	}
	var gotEnabled, gotDisabled bool
	for _, id := range ids {
		switch id {
		case enabledCh:
			gotEnabled = true
		case disabledCh:
			gotDisabled = true
		default:
			t.Fatalf("MonitorChannelIDs вернул непривязанный канал %d: %v", id, ids)
		}
	}
	if !gotEnabled || !gotDisabled {
		t.Fatalf("MonitorChannelIDs = %v, want [%d %d]", ids, enabledCh, disabledCh)
	}
}

// TestOutboxNotifierAllOwnChannelsDisabled фиксирует суть правки: монитор,
// у которого ВСЕ собственные каналы выключены, не должен уведомлять никуда —
// и уж точно не должен рассылать по всем каналам проекта.
func TestOutboxNotifierAllOwnChannelsDisabled(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	asvc := alert.NewService(pool)
	usvc := uptime.NewService(pool)
	pid := newProject(t, pool)

	offCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off",
	})
	if err != nil {
		t.Fatalf("CreateChannel disabled: %v", err)
	}
	// Канал проекта, НЕ привязанный к монитору: именно он уезжал бы по фолбэку.
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/project-wide",
	}); err != nil {
		t.Fatalf("CreateChannel project-wide: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{offCh})

	outbox := notify.NewOutbox(pool)
	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: outbox, BaseURL: "http://localhost"}
	if err := n.Notify(ctx, uptime.Event{Kind: "down", Monitor: m}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	var jobs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM notification_outbox o
		JOIN alert_channels c ON c.id = o.channel_id
		WHERE c.project_id = $1`, pid).Scan(&jobs); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("поставлено %d заданий, ожидалось 0: выключенный собственный канал не должен включать рассылку по каналам проекта", jobs)
	}
}

// Трансграничный гейт: при политике без доверия получателю во внешние каналы не должно
// уезжать имя монитора/причина падения (тело/subject/monitor_name/cause);
// при true — уезжает, как раньше.
func TestOutboxNotifierExternalDetailsGate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	t.Run("withheld when false", func(t *testing.T) {
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

		n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, false)}
		if err := n.Notify(ctx, uptime.Event{
			Kind: "down", Monitor: m, Regions: []string{"eu"}, Cause: "connection refused",
		}); err != nil {
			t.Fatalf("Notify: %v", err)
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
			if _, ok := p["monitor_name"]; ok {
				t.Errorf("channel %d leaks monitor_name: %+v", id, p)
			}
			if _, ok := p["cause"]; ok {
				t.Errorf("channel %d leaks cause: %+v", id, p)
			}
			if body, _ := p["body"].(string); strings.Contains(body, "API health") || strings.Contains(body, "connection refused") {
				t.Errorf("channel %d body leaks details: %q", id, body)
			}
			if subj, _ := p["subject"].(string); strings.Contains(subj, "API health") {
				t.Errorf("channel %d subject leaks name: %q", id, subj)
			}
			if p["url"] == nil {
				t.Errorf("channel %d lost url: %+v", id, p)
			}
		}
		if _, ok := byChannel[telegramCh].Payload["secret"]; ok {
			t.Errorf("секрет попал в payload очереди: %+v", byChannel[telegramCh].Payload)
		}
	})

	t.Run("delivered when true", func(t *testing.T) {
		pid := newProject(t, pool)
		webhookCh, err := asvc.CreateChannel(ctx, alert.Channel{
			ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
		})
		if err != nil {
			t.Fatalf("CreateChannel webhook: %v", err)
		}
		m := newNotifierMonitor(t, usvc, pid, []int64{webhookCh})

		n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
		if err := n.Notify(ctx, uptime.Event{
			Kind: "down", Monitor: m, Regions: []string{"eu"}, Cause: "connection refused",
		}); err != nil {
			t.Fatalf("Notify: %v", err)
		}
		jobs, err := ob.Claim(ctx, 10)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("jobs = %+v err=%v, want 1", jobs, err)
		}
		if jobs[0].Payload["monitor_name"] != "API health" || jobs[0].Payload["cause"] != "connection refused" {
			t.Errorf("external details missing with an allowing policy: %+v", jobs[0].Payload)
		}
	})
}

// TestOutboxNotifierOwnChannelRoutingAndNoSecretInQueue закрывает два дефекта
// сразу, оба про собственные каналы монитора при включённом мастер-ключе.
//
// Первый: uptime читал alert_channels.secret своим запросом, а ключ есть
// только у alert.Service — расшифровать было нечем, и в Telegram уезжала
// строка "enc:AAAA…" в качестве токена бота. Молча: попытки расшифровать не
// было, значит не было и ошибки в логе.
//
// Второй: секрет клали в notification_outbox.payload — обычный jsonb, — и
// `SELECT payload->>'secret'` отдавал живые токены за всё окно хранения
// очереди, обесценивая шифрование at-rest целиком.
//
// Поэтому здесь проверяется, что задача уходит именно в СВОЙ канал монитора,
// что секрета в очереди нет вовсе, и что по channel_id секрет достаётся
// расшифрованным — тем самым путём, которым его берёт notify.Worker.
func TestOutboxNotifierOwnChannelRoutingAndNoSecretInQueue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	asvc.SetSecretKey("test-master-key-at-least-32-bytes-long")
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const botToken = "123456:AAHplaintext-bot-token"
	pid := newProject(t, pool)
	ownCh, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelTelegram, Enabled: true,
		Target: "-100500", Secret: botToken,
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Секрет обязан лежать в базе зашифрованным — иначе тест ничего не proves.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT secret FROM alert_channels WHERE id = $1`, ownCh).Scan(&stored); err != nil {
		t.Fatalf("read stored secret: %v", err)
	}
	if !strings.HasPrefix(stored, "enc:") {
		t.Fatalf("secret at rest = %q, want enc:-prefixed ciphertext", stored)
	}

	m := newNotifierMonitor(t, usvc, pid, []int64{ownCh})
	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
	if err := n.Notify(ctx, uptime.Event{Kind: "down", Monitor: m}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v err=%v, want exactly 1 job", jobs, err)
	}
	if jobs[0].ChannelID != ownCh {
		t.Fatalf("job ушла в канал %d, want собственный канал монитора %d", jobs[0].ChannelID, ownCh)
	}
	if _, ok := jobs[0].Payload["secret"]; ok {
		t.Fatalf("секрет попал в payload очереди: %+v", jobs[0].Payload)
	}

	// Путь доставки: воркер спрашивает секрет по channel_id и получает
	// расшифрованный токен, а не хранимый шифротекст.
	secret, err := asvc.ChannelSecret(ctx, jobs[0].ChannelID)
	if err != nil {
		t.Fatalf("ChannelSecret: %v", err)
	}
	if secret != botToken {
		t.Fatalf("ChannelSecret = %q, want расшифрованный %q (шифротекст в доставке = канал не работает никогда)", secret, botToken)
	}
}

// TestOutboxNotifierNotifyStepFiltersAndReturnsEnqueued — W2-C находка 2:
// NotifyStep перезагружает инцидент+монитор по ID (Scheduler знает только
// incidentID), фильтрует каналы по ladder[level].ChannelIDs ПОСЛЕ
// Deliverable-гейта (как остальные 5 нотифаеров) и возвращает РЕАЛЬНО
// заенкенные каналы, не полный список каналов монитора.
func TestOutboxNotifierNotifyStepFiltersAndReturnsEnqueued(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	ch1, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook1",
	})
	if err != nil {
		t.Fatalf("CreateChannel ch1: %v", err)
	}
	ch2, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook2",
	})
	if err != nil {
		t.Fatalf("CreateChannel ch2: %v", err)
	}
	m := newNotifierMonitor(t, usvc, pid, []int64{ch1, ch2})

	inc, created, err := usvc.OpenIncident(ctx, m.ID, "connection refused", []string{"eu"}, false)
	if err != nil || !created {
		t.Fatalf("OpenIncident: (%+v,%v,%v)", inc, created, err)
	}

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}

	enqueued, err := n.NotifyStep(ctx, inc.ID, []int64{ch1}, 1)
	if err != nil {
		t.Fatalf("NotifyStep: %v", err)
	}
	if len(enqueued) != 1 || enqueued[0] != ch1 {
		t.Fatalf("enqueued = %v, want [%d]", enqueued, ch1)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("len(jobs) = %d, want 1 (только ch1, ch2 отфильтрован лесенкой)", len(jobs))
	}
	if jobs[0].ChannelID != ch1 {
		t.Fatalf("job.ChannelID = %d, want %d", jobs[0].ChannelID, ch1)
	}
	if jobs[0].Payload["kind"] != "down" || jobs[0].Payload["cause"] != "connection refused" {
		t.Fatalf("job payload = %+v, want kind=down cause=connection refused (перезагружено из инцидента по ID)", jobs[0].Payload)
	}
}

// TestOutboxNotifierNotifyStepUnknownIncidentErrors — NotifyStep обязан
// вернуть ошибку, а не молча ничего не сделать, если incidentID не
// резолвится (например, инцидент был удалён между постановкой шага в
// планировщике и его исполнением).
func TestOutboxNotifierNotifyStepUnknownIncidentErrors(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	n := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
	if _, err := n.NotifyStep(ctx, 999999999, nil, 0); err == nil {
		t.Fatalf("NotifyStep(несуществующий incidentID) = nil error, want ошибку")
	}
}
