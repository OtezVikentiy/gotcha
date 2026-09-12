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

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)
	assertOpenIncident(t, ctx, usvc, mon.ID)
	if _, err := ob.Claim(ctx, 10); err != nil {
		t.Fatalf("claim down job: %v", err)
	}

	neverSawDown, err := asvc.CreateChannel(ctx, alert.Channel{ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/late"})
	if err != nil {
		t.Fatalf("CreateChannel neverSawDown: %v", err)
	}

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

// claim (incident_escalations) идёт до отправки — недоступность таблицы
// клейма обязана отложить уведомление, а не отправить его в обход клейма.
func TestDetectorRetriesNotifyOpenAfterClaimFailure(t *testing.T) {
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

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)
	inc := assertOpenIncident(t, ctx, usvc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: claim на incident_escalations должен был провалиться до отправки")
	}
	if !inc.NotifyOpenFailed || inc.NotifyOpenAttempts != 1 {
		t.Fatalf("NotifyOpenFailed=%v NotifyOpenAttempts=%d, want true/1", inc.NotifyOpenFailed, inc.NotifyOpenAttempts)
	}

	var loggedCount int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1",
		inc.ID).Scan(&loggedCount); err != nil {
		t.Fatalf("select incident_escalations: %v", err)
	}
	if loggedCount != 0 {
		t.Fatalf("incident_escalations rows = %d, want 0 (клейм должен был провалиться целиком)", loggedCount)
	}

	downJobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim down jobs: %v", err)
	}
	if len(downJobs) != 0 {
		t.Fatalf("down jobs = %d, want 0: без выигранного клейма отправки не должно было быть", len(downJobs))
	}

	dropConstraint()
	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC().Add(time.Second), nil)
	inc = assertOpenIncident(t, ctx, usvc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false после ретрая, want true")
	}

	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='uptime' AND incident_id=$1 AND step=0 AND channel_id=$2",
		inc.ID, ch).Scan(&loggedCount); err != nil {
		t.Fatalf("select incident_escalations after retry: %v", err)
	}
	if loggedCount != 1 {
		t.Fatalf("incident_escalations rows after retry = %d, want 1", loggedCount)
	}

	downJobs, err = ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim down jobs after retry: %v", err)
	}
	if len(downJobs) != 1 {
		t.Fatalf("down jobs после ретрая = %d, want ровно 1", len(downJobs))
	}

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

// Отправка проходит, а MarkNotified проваливается — детектор обязан завести
// это как провал (для ретрая), но не переслать уже ушедшее уведомление снова.
func TestNotifyOpenMarksFailedWithoutDuplicateWhenMarkNotifiedFails(t *testing.T) {
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

	if _, err := pool.Exec(ctx, "ALTER TABLE incidents ADD CONSTRAINT test_force_mark_notified_fail CHECK (NOT notified_open)"); err != nil {
		t.Fatalf("add forcing constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			"ALTER TABLE incidents DROP CONSTRAINT IF EXISTS test_force_mark_notified_fail"); err != nil {
			t.Errorf("drop forcing constraint: %v", err)
		}
	})

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", time.Now().UTC(), nil)

	inc := assertOpenIncident(t, ctx, usvc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: MarkNotified должен был провалиться на constraint")
	}
	if !inc.NotifyOpenFailed {
		t.Fatalf("NotifyOpenFailed = false, want true")
	}
	if inc.NotifyOpenAttempts != 1 {
		t.Fatalf("NotifyOpenAttempts = %d, want 1", inc.NotifyOpenAttempts)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want ровно 1: отправка прошла до провала MarkNotified, дубля быть не должно", len(jobs))
	}
	if jobs[0].ChannelID != ch {
		t.Fatalf("job channel = %d, want %d", jobs[0].ChannelID, ch)
	}
}

// Закрытый инцидент — новая строка с новым id; идемпотентный ключ несёт
// этот id и не должен подавить уведомление по новому падению.
func TestNotifyOpenSendsAgainAfterIncidentReopens(t *testing.T) {
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
	now := time.Now().UTC()

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom", now, nil)
	first := assertOpenIncident(t, ctx, usvc, mon.ID)
	if !first.NotifiedOpen {
		t.Fatalf("first incident: NotifiedOpen = false, want true")
	}
	firstJobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim first down jobs: %v", err)
	}
	if len(firstJobs) != 1 {
		t.Fatalf("first down jobs = %d, want 1", len(firstJobs))
	}

	applyAndDetect(t, ctx, usvc, d, mon, "local", true, "", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, usvc, mon.ID)
	if _, err := ob.Claim(ctx, 10); err != nil { // осушаем recovery-job, он не по счёту этого теста
		t.Fatalf("claim recovery job: %v", err)
	}

	applyAndDetect(t, ctx, usvc, d, mon, "local", false, "boom again", now.Add(2*time.Second), nil)
	second := assertOpenIncident(t, ctx, usvc, mon.ID)
	if second.ID == first.ID {
		t.Fatalf("second incident id = %d, want a new id distinct from the first (%d)", second.ID, first.ID)
	}
	if !second.NotifiedOpen {
		t.Fatalf("second incident: NotifiedOpen = false, want true — reopen must page again")
	}

	secondJobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim second down jobs: %v", err)
	}
	if len(secondJobs) != 1 {
		t.Fatalf("second down jobs = %d, want 1 (переоткрытие обязано пейджить снова, не молчать)", len(secondJobs))
	}
	if secondJobs[0].ChannelID != ch {
		t.Fatalf("second job channel = %d, want %d", secondJobs[0].ChannelID, ch)
	}
}
