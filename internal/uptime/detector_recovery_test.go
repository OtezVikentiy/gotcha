package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/i18n"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestDetectorLogsStepZeroForDownDelivery — W3-E, кластер 4 находка 3:
// доставка "down" уровня 0 (Detector.notifyOpen, минуя escalation.
// SendStepIfDue — см. её докблок про OpenUnacked) обязана залогировать
// РЕАЛЬНО заенкененный канал в incident_escalations как шаг 0 сама, иначе
// escalation.RecoveryChannels не находит ничего для инцидента, ни разу не
// дошедшего до эскалации уровня 1, и адресный "up" не уходит НИКОМУ.
func TestDetectorLogsStepZeroForDownDelivery(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	ch, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	mon := createMonitor(t, usvc, pid, 1, 1)

	notifier := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
	d := &uptime.Detector{Svc: usvc, Notifier: notifier, Pool: pool}

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)
	inc := assertOpenIncident(t, ctx, usvc, mon.ID)

	var step int
	var loggedChannel int64
	if err := pool.QueryRow(ctx,
		"SELECT step, channel_id FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1",
		inc.ID).Scan(&step, &loggedChannel); err != nil {
		t.Fatalf("select incident_escalations: %v (step 0 must be logged by notifyOpen)", err)
	}
	if step != 0 {
		t.Errorf("logged step = %d, want 0", step)
	}
	if loggedChannel != ch {
		t.Errorf("logged channel = %d, want %d (the one actually enqueued)", loggedChannel, ch)
	}
}

// TestDetectorRecoveryAddressedOnlyToChannelsThatSawDown — W3-E, кластер 4
// находка 3: "up" уходит ТОЛЬКО каналам, которые реально видели "down" этого
// инцидента (escalation.RecoveryChannels), а не всем ТЕКУЩИМ каналам проекта
// заново. Канал, добавленный ПОСЛЕ открытия инцидента (никогда не видевший
// тревогу), не должен первым увидеть «инцидент закрыт».
func TestDetectorRecoveryAddressedOnlyToChannelsThatSawDown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	sawDown, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/saw-down"})
	if err != nil {
		t.Fatalf("CreateChannel sawDown: %v", err)
	}
	mon := createMonitor(t, usvc, pid, 1, 1)

	notifier := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
	d := &uptime.Detector{Svc: usvc, Notifier: notifier, Pool: pool}

	// down: только sawDown существует, только он получает "down" и лог шага 0.
	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)
	assertOpenIncident(t, ctx, usvc, mon.ID)
	if _, err := ob.Claim(ctx, 10); err != nil {
		t.Fatalf("claim down job: %v", err)
	}

	// Канал добавлен ПОСЛЕ открытия инцидента — никогда не видел тревогу.
	neverSawDown, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/late"})
	if err != nil {
		t.Fatalf("CreateChannel neverSawDown: %v", err)
	}

	// up: recovery_threshold=1 — один "ok" уже разрешает инцидент.
	applyAndDetect(t, ctx, usvc, d, mon, "local", true, "", time.Now().UTC().Add(time.Minute), nil)
	assertNoOpenIncident(t, ctx, usvc, mon.ID)

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim up jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("up jobs = %d, want exactly 1 (только канал, видевший down)", len(jobs))
	}
	if jobs[0].ChannelID != sawDown {
		t.Fatalf("up job channel = %d, want %d (sawDown) — got %d (neverSawDown) instead", jobs[0].ChannelID, sawDown, neverSawDown)
	}
	if jobs[0].Payload["kind"] != "up" {
		t.Errorf("kind = %v, want up", jobs[0].Payload["kind"])
	}
}

// TestDetectorRecoverySilentWhenNoChannelSawDown — M-7 брифа Task 6: пустой
// набор RecoveryChannels — молчание, а не откат на «все каналы проекта».
// Симулируется, отключив единственный канал ДО открытия инцидента: "down"
// не доставляется никому (Deliverable=false), поэтому incident_escalations
// остаётся пустым для этого инцидента, и recovery не должно уйти вообще.
func TestDetectorRecoverySilentWhenNoChannelSawDown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	if _, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: false, Target: "https://example.com/off"}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	mon := createMonitor(t, usvc, pid, 1, 1)

	notifier := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
	d := &uptime.Detector{Svc: usvc, Notifier: notifier, Pool: pool}

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)
	inc := assertOpenIncident(t, ctx, usvc, mon.ID)
	// "down" не доставлен никуда (единственный канал выключен, значит не
	// Deliverable) — notifyOpen получает пустой enqueued от Dispatch и
	// логирует шаг 0 НИКОМУ. Страховка теста: incident_escalations пуст для
	// этого инцидента, иначе тест проверял бы не тот сценарий.
	var loggedCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1",
		inc.ID).Scan(&loggedCount); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if loggedCount != 0 {
		t.Fatalf("test setup: incident_escalations rows = %d, want 0 (единственный канал выключен, значит недоставляем)", loggedCount)
	}

	applyAndDetect(t, ctx, usvc, d, mon, "local", true, "", time.Now().UTC().Add(time.Minute), nil)
	assertNoOpenIncident(t, ctx, usvc, mon.ID)

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none: unnotified open must not send recovery either", jobs)
	}
}

// TestDetectorRetriesStepZeroLogAfterTransientFailure — W3-E, кластер 4
// (правка по вердикту ревью): у остальных пяти источников провал LogStep
// ретраится SendStepIfDue с maxLogFailureAttempts попытками (W2-C находка
// 3); у аптайма шаг 0 доставляет сам Detector, минуя лесенку, и ДО этой
// правки провал LogStep не ретраился вовсе — recovery уходило бы в пустоту
// молча навсегда. Проверяет всю цепочку: LogStep форсированно падает
// (CHECK(false) на incident_escalations — тот же приём, что
// TestSendStepIfDueForcesBumpAfterMaxLogFailures в internal/escalation/
// step_internal_test.go, константный блокирует даже суперпользователя
// тестового контейнера) → строка появляется в escalation_step_log_failures
// и снимок канала — в incidents.notify_open_channels → ограничение
// снимается → следующий тик (settleHeldIncident, ещё "down") добирает лог,
// НЕ переотправляя "down" → recovery на "up" уходит адресно каналу, который
// реально видел "down", а не в пустоту.
func TestDetectorRetriesStepZeroLogAfterTransientFailure(t *testing.T) {
	pool := testenv.MigratedPG(t)
	usvc := uptime.NewService(pool)
	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	ch, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	mon := createMonitor(t, usvc, pid, 1, 1)

	notifier := &uptime.OutboxNotifier{Alerts: asvc, Uptime: usvc, Outbox: ob, BaseURL: "https://gotcha.example", Details: alert.NewDetailPolicy("", nil, true), Locale: i18n.Locale{Code: "en"}}
	d := &uptime.Detector{Svc: usvc, Notifier: notifier, Pool: pool}

	if _, err := pool.Exec(ctx, "ALTER TABLE incident_escalations ADD CONSTRAINT test_force_log_fail CHECK (false)"); err != nil {
		t.Fatalf("add forcing constraint: %v", err)
	}
	dropped := false
	dropConstraint := func() {
		if dropped {
			return
		}
		if _, err := pool.Exec(context.Background(), "ALTER TABLE incident_escalations DROP CONSTRAINT IF EXISTS test_force_log_fail"); err != nil {
			t.Fatalf("drop forcing constraint: %v", err)
		}
		dropped = true
	}
	t.Cleanup(dropConstraint)

	// down: доставка успевает (Outbox — другая таблица, constraint её не
	// трогает), лог шага 0 — нет.
	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)
	inc := assertOpenIncident(t, ctx, usvc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("test setup: NotifiedOpen = false, want true (доставка не должна была пострадать от constraint на ДРУГОЙ таблице)")
	}

	var loggedCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1",
		inc.ID).Scan(&loggedCount); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if loggedCount != 0 {
		t.Fatalf("test setup: incident_escalations rows = %d, want 0 (LogStep должен был провалиться)", loggedCount)
	}

	var attempts int
	if err := pool.QueryRow(ctx,
		"SELECT attempts FROM escalation_step_log_failures WHERE incident_source='uptime' AND incident_id=$1 AND step=0",
		inc.ID).Scan(&attempts); err != nil {
		t.Fatalf("select escalation_step_log_failures: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}

	var pending []int64
	if err := pool.QueryRow(ctx, "SELECT notify_open_channels FROM incidents WHERE id=$1", inc.ID).Scan(&pending); err != nil {
		t.Fatalf("select notify_open_channels: %v", err)
	}
	if len(pending) != 1 || pending[0] != ch {
		t.Fatalf("notify_open_channels = %v, want [%d] (снимок должен пережить провал лога)", pending, ch)
	}

	// "БД восстановилась" — снимаем ограничение и даём Detector'у ещё один
	// тик на ТОМ ЖЕ открытом инциденте (still down): settleHeldIncident
	// увидит NotifiedOpen=true и обязан добрать лог, не переотправляя "down".
	dropConstraint()
	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC().Add(time.Second), nil)

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1 AND step=0 AND channel_id=$2",
		inc.ID, ch).Scan(&loggedCount); err != nil {
		t.Fatalf("select incident_escalations after retry: %v", err)
	}
	if loggedCount != 1 {
		t.Fatalf("incident_escalations rows after retry = %d, want 1 (ретрай обязан был дописать шаг 0)", loggedCount)
	}

	var remainingFailures int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM escalation_step_log_failures WHERE incident_source='uptime' AND incident_id=$1 AND step=0",
		inc.ID).Scan(&remainingFailures); err != nil {
		t.Fatalf("select escalation_step_log_failures after retry: %v", err)
	}
	if remainingFailures != 0 {
		t.Errorf("escalation_step_log_failures rows after retry = %d, want 0 (сброшено)", remainingFailures)
	}

	var pendingAfter []int64
	if err := pool.QueryRow(ctx, "SELECT notify_open_channels FROM incidents WHERE id=$1", inc.ID).Scan(&pendingAfter); err != nil {
		t.Fatalf("select notify_open_channels after retry: %v", err)
	}
	if pendingAfter != nil {
		t.Errorf("notify_open_channels after retry = %v, want NULL (очищено)", pendingAfter)
	}

	// Не переотправили "down": ровно ОДНА задача в очереди — та, что ушла
	// на самом первом тике.
	downJobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim down jobs: %v", err)
	}
	if len(downJobs) != 1 {
		t.Fatalf("down jobs = %d, want exactly 1 (ретрай лога не должен переотправлять \"down\")", len(downJobs))
	}

	// up: recovery_threshold=1 — теперь recovery должно уйти адресно.
	applyAndDetect(t, ctx, usvc, d, mon, "local", true, "", time.Now().UTC().Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, usvc, mon.ID)

	upJobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim up jobs: %v", err)
	}
	if len(upJobs) != 1 || upJobs[0].ChannelID != ch {
		t.Fatalf("up jobs = %+v, want exactly 1 for channel %d", upJobs, ch)
	}
}
