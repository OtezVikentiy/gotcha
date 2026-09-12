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

	downJobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("claim down jobs: %v", err)
	}
	if len(downJobs) != 1 {
		t.Fatalf("down jobs = %d, want exactly 1 (ретрай лога не должен переотправлять \"down\")", len(downJobs))
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
