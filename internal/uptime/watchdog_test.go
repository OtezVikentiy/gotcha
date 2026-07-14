package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// baseHeartbeatMonitor creates a kind=heartbeat monitor with the given
// fail_threshold and grace period — mirrors baseHTTPMonitor/createMonitor
// (monitor_test.go, state_test.go) for the watchdog-specific kind.
func baseHeartbeatMonitor(t *testing.T, projectID int64, failThreshold, graceSeconds int) uptime.Monitor {
	t.Helper()
	return uptime.Monitor{
		ProjectID:         projectID,
		Name:              "heartbeat",
		Kind:              uptime.KindHeartbeat,
		Enabled:           true,
		IntervalSeconds:   60,
		TimeoutSeconds:    10,
		FailThreshold:     failThreshold,
		RecoveryThreshold: 1,
		Consensus:         uptime.ConsensusMajority,
		Config:            heartbeatConfig(t, uptime.HeartbeatConfig{GraceSeconds: graceSeconds}),
	}
}

// fastWatchdog builds a Watchdog with tickers fast enough for tests
// (mirrors newFastRunner in runner_test.go).
func fastWatchdog(svc *uptime.Service, d *uptime.Detector, n uptime.Notifier) *uptime.Watchdog {
	return &uptime.Watchdog{
		Svc:      svc,
		Detector: d,
		Notifier: n,
		Region:   "local",
		Interval: 20 * time.Millisecond,
		SSLEvery: 20 * time.Millisecond,
	}
}

func TestWatchdogHeartbeatOpensIncidentOnStaleBeat(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	// fail_threshold=1 — a single missed-beat tick is enough to reach "down"
	// (see task brief: "один тик = одна неудача").
	m := baseHeartbeatMonitor(t, pid, 1, 60)
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	wd := fastWatchdog(svc, d, notifier)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})

	inc := assertOpenIncident(t, ctx, svc, created.ID)
	if inc.Cause == "" {
		t.Fatalf("Incident.Cause is empty, want a missed-heartbeat message")
	}
	downEvents := notifier.kindEvents("down")
	if len(downEvents) != 1 {
		t.Fatalf("down events = %d, want 1: %+v", len(downEvents), notifier.Events())
	}
}

func TestWatchdogHeartbeatFreshBeatDoesNothing(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHeartbeatMonitor(t, pid, 1, 60)
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() WHERE id = $1", created.ID); err != nil {
		t.Fatalf("set last_beat_at: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	wd := fastWatchdog(svc, d, notifier)

	wctx, wcancel := context.WithCancel(ctx)
	go wd.Run(wctx)
	// Give the fast ticker a handful of chances to (wrongly) act, then stop.
	time.Sleep(150 * time.Millisecond)
	wcancel()

	states, err := svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("States = %+v, want none (fresh beat, watchdog must not touch it)", states)
	}
	assertNoOpenIncident(t, ctx, svc, created.ID)
	if len(notifier.Events()) != 0 {
		t.Fatalf("notifier.Events() = %+v, want none", notifier.Events())
	}
}

func TestWatchdogSSLExpiringNotifiesLargestUnalertedThresholdOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.SSLAlertDays = 14
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	// Pin daysLeft deterministically to 5: ceil((expires-now)/24h) == 5 for
	// anything in (4d, 5d] from now.
	expires := time.Now().UTC().Add(4*24*time.Hour + 12*time.Hour)
	if err := svc.SetSSLExpiry(ctx, created.ID, expires); err != nil {
		t.Fatalf("SetSSLExpiry: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	wd := fastWatchdog(svc, d, notifier)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	waitForRunner(t, func() bool {
		return len(notifier.kindEvents("ssl_expiring")) >= 1
	})
	// Give a few more fast ticks a chance to (wrongly) double-fire before we
	// assert the final count.
	time.Sleep(150 * time.Millisecond)

	events := notifier.kindEvents("ssl_expiring")
	if len(events) != 1 {
		t.Fatalf("ssl_expiring events = %d, want 1: %+v", len(events), events)
	}
	if events[0].DaysLeft != 5 {
		t.Fatalf("DaysLeft = %d, want 5", events[0].DaysLeft)
	}

	var alerted []int
	if err := pool.QueryRow(ctx, "SELECT ssl_alerted_days FROM monitors WHERE id = $1", created.ID).Scan(&alerted); err != nil {
		t.Fatalf("select ssl_alerted_days: %v", err)
	}
	alertedSet := map[int]bool{}
	for _, d := range alerted {
		alertedSet[d] = true
	}
	// daysLeft=5 satisfies both the 14 (ssl_alert_days) and the built-in 7
	// thresholds at once — both get recorded from a single notification so
	// a later tick at the same daysLeft doesn't re-fire for 7.
	if !alertedSet[14] || !alertedSet[7] {
		t.Fatalf("ssl_alerted_days = %v, want it to contain 14 and 7", alerted)
	}

	wcancel()

	// A day later (simulated): daysLeft drops to 3, crossing the built-in 3
	// threshold — a new, single notification.
	expires3 := time.Now().UTC().Add(2*24*time.Hour + 12*time.Hour)
	if err := svc.SetSSLExpiry(ctx, created.ID, expires3); err != nil {
		t.Fatalf("SetSSLExpiry (day later): %v", err)
	}
	// SetSSLExpiry only clears ssl_alerted_days when the new expiry is
	// LATER than the stored one (a fresh cert) — here it's earlier, so the
	// previously recorded {14,7} survive, as intended.

	wd2 := fastWatchdog(svc, d, notifier)
	wctx2, wcancel2 := context.WithCancel(ctx)
	defer wcancel2()
	go wd2.Run(wctx2)

	waitForRunner(t, func() bool {
		return len(notifier.kindEvents("ssl_expiring")) >= 2
	})
	time.Sleep(150 * time.Millisecond)
	wcancel2()

	events = notifier.kindEvents("ssl_expiring")
	if len(events) != 2 {
		t.Fatalf("ssl_expiring events after day-later tick = %d, want 2: %+v", len(events), events)
	}
	if events[1].DaysLeft != 3 {
		t.Fatalf("second event DaysLeft = %d, want 3", events[1].DaysLeft)
	}
}

func TestWatchdogReminderNotifiesAndTouchesOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 1
	m.RemindEveryMinutes = 10
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	applyAndDetect(t, ctx, svc, d, created, "local", false, "boom", time.Now().UTC(), nil)
	assertOpenIncident(t, ctx, svc, created.ID)

	// Backdate the incident so it's already 30 minutes old — remind_every=10
	// means it's due immediately.
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 minutes' WHERE monitor_id = $1 AND resolved_at IS NULL",
		created.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}

	wd := fastWatchdog(svc, d, notifier)
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	waitForRunner(t, func() bool {
		return len(notifier.kindEvents("reminder")) >= 1
	})
	// A handful more fast ticks: last_reminded_at should now be "now", so
	// remind_every=10 keeps it from firing again for a long time — no
	// second reminder should show up.
	time.Sleep(150 * time.Millisecond)
	wcancel()

	reminders := notifier.kindEvents("reminder")
	if len(reminders) != 1 {
		t.Fatalf("reminder events = %d, want 1: %+v", len(reminders), reminders)
	}
	if reminders[0].DurationSeconds < 30*60 {
		t.Fatalf("DurationSeconds = %d, want >= 1800 (30 minutes)", reminders[0].DurationSeconds)
	}

	inc := assertOpenIncident(t, ctx, svc, created.ID)
	if inc.LastRemindedAt == nil {
		t.Fatalf("LastRemindedAt is nil, want it set after the reminder watchdog ran")
	}
	if time.Since(*inc.LastRemindedAt) > time.Minute {
		t.Fatalf("LastRemindedAt = %v, want it recent", inc.LastRemindedAt)
	}
}

// TestWatchdogNilNotifierDoesNotMarkDelivered — in "incidents only, no
// notifications" mode (Watchdog.Notifier == nil), checkSSL/checkReminders
// must not record ssl_alerted_days/last_reminded_at either: doing so would
// permanently swallow the alert once a real Notifier is configured later,
// since the threshold/reminder would already look "delivered".
func TestWatchdogNilNotifierDoesNotMarkDelivered(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)

	// SSL side: a monitor whose cert is already within every threshold.
	sslMon := baseHTTPMonitor(pid)
	sslMon.SSLAlertDays = 14
	sslMon.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	createdSSL := mustCreateMonitor(t, svc, ctx, sslMon, []string{"local"})
	expires := time.Now().UTC().Add(4*24*time.Hour + 12*time.Hour) // daysLeft == 5
	if err := svc.SetSSLExpiry(ctx, createdSSL.ID, expires); err != nil {
		t.Fatalf("SetSSLExpiry: %v", err)
	}

	// Reminder side: an open incident already due for a reminder.
	remMon := baseHTTPMonitor(pid)
	remMon.FailThreshold = 1
	remMon.RemindEveryMinutes = 10
	remMon.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	createdRem := mustCreateMonitor(t, svc, ctx, remMon, []string{"local"})
	d := &uptime.Detector{Svc: svc, Notifier: nil}
	applyAndDetect(t, ctx, svc, d, createdRem, "local", false, "boom", time.Now().UTC(), nil)
	assertOpenIncident(t, ctx, svc, createdRem.ID)
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 minutes' WHERE monitor_id = $1 AND resolved_at IS NULL",
		createdRem.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}

	// wd.Notifier is deliberately nil — the field's zero value, matching the
	// "incidents only" deployment mode.
	wd := fastWatchdog(svc, d, nil)
	wctx, wcancel := context.WithCancel(ctx)
	go wd.Run(wctx)
	time.Sleep(200 * time.Millisecond)
	wcancel()

	var alerted []int
	if err := pool.QueryRow(ctx, "SELECT ssl_alerted_days FROM monitors WHERE id = $1", createdSSL.ID).Scan(&alerted); err != nil {
		t.Fatalf("select ssl_alerted_days: %v", err)
	}
	if len(alerted) != 0 {
		t.Fatalf("ssl_alerted_days = %v, want empty (nil Notifier must not mark as delivered)", alerted)
	}

	inc := assertOpenIncident(t, ctx, svc, createdRem.ID)
	if inc.LastRemindedAt != nil {
		t.Fatalf("LastRemindedAt = %v, want nil (nil Notifier must not mark as delivered)", inc.LastRemindedAt)
	}
}
