package uptime_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	wd := fastWatchdog(svc, d, notifier)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	// Ждём именно ИНЦИДЕНТА, а не состояния. Состояние монитора пишется на шаг
	// раньше открытия инцидента, и ожидание по нему возвращало управление в
	// промежутке между двумя записями: тест шёл проверять инцидент, которого
	// ещё секунду не будет. Гонка была латентной и вылезла, когда порядок
	// работ в Run сдвинулся на пару миллисекунд.
	waitForRunner(t, func() bool {
		_, found, err := svc.OpenIncidentFor(context.Background(), created.ID)
		return err == nil && found
	})
	states, err := svc.States(ctx, created.ID)
	if err != nil || len(states) != 1 || states[0].Status != "down" {
		t.Fatalf("States = %+v err=%v, want single down state", states, err)
	}

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
	fresh := mustCreateMonitor(t, pool, svc, ctx, baseHeartbeatMonitor(t, pid, 1, 60), []string{"local"})
	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() WHERE id = $1", fresh.ID); err != nil {
		t.Fatalf("set last_beat_at: %v", err)
	}

	// Позитивный контроль: второй монитор с ПРОТУХШИМ ударом. Раньше тест
	// запускал watchdog, спал и утверждал, что ничего не произошло — и оставался
	// зелёным, даже если бы watchdog не тикнул ни разу (сломанный тикер,
	// упавший Run, изменившийся запрос выборки). «Ничего не произошло» — это
	// утверждение о бездействии, и оно чего-то стоит только рядом с
	// доказательством, что действовать было кому.
	stale := mustCreateMonitor(t, pool, svc, ctx, baseHeartbeatMonitor(t, pid, 1, 60), []string{"local"})
	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", stale.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	wd := fastWatchdog(svc, d, notifier)

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	// Ждём срабатывания по протухшему — этим и доказано, что тик состоялся.
	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), stale.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})
	wcancel()

	states, err := svc.States(ctx, fresh.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("States = %+v, want none (fresh beat, watchdog must not touch it)", states)
	}
	assertNoOpenIncident(t, ctx, svc, fresh.ID)
	for _, e := range notifier.Events() {
		if e.Monitor.ID == fresh.ID {
			t.Fatalf("notifier got %+v for the fresh monitor, want nothing", e)
		}
	}
}

// TestStaleHeartbeatsPopulatesRegionCount: StaleHeartbeats must fill in
// Regions/RegionCount like Get/List do (finding P1-3) — checkHeartbeats feeds
// its result straight into Detector.OnResult/aggregate(), which uses
// RegionCount as consensus's denominator. Left at zero, a multi-region
// heartbeat monitor's chosen consensus (all/majority) silently degraded to
// "whichever region's watchdog reports first" instead of waiting for every
// configured region.
func TestStaleHeartbeatsPopulatesRegionCount(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHeartbeatMonitor(t, pid, 1, 60)
	m.Consensus = uptime.ConsensusAll
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local", "eu"})
	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	stale, err := svc.StaleHeartbeats(ctx)
	if err != nil {
		t.Fatalf("StaleHeartbeats: %v", err)
	}
	var found *uptime.Monitor
	for i := range stale {
		if stale[i].ID == created.ID {
			found = &stale[i]
		}
	}
	if found == nil {
		t.Fatalf("StaleHeartbeats did not return monitor %d among %+v", created.ID, stale)
	}
	if found.RegionCount != 2 {
		t.Errorf("RegionCount = %d, want 2 (local+eu)", found.RegionCount)
	}
	if len(found.Regions) != 2 {
		t.Errorf("Regions = %+v, want 2 entries", found.Regions)
	}
}

// TestWatchdogHeartbeatConsensusAllWaitsForAllRegions: a two-region
// deployment (two Watchdog processes, one per region — cfg.LocalRegion in
// cmd/gotcha, see checkSSL's doc comment) with consensus=all must NOT open
// an incident off just one region's report. Before the RegionCount fix,
// StaleHeartbeats left RegionCount at 0, so aggregate() fell back to
// total=decided and "all" was satisfied (down==total==1) the instant the
// FIRST region's watchdog ticked — defeating the point of choosing "all".
func TestWatchdogHeartbeatConsensusAllWaitsForAllRegions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHeartbeatMonitor(t, pid, 1, 60)
	m.Consensus = uptime.ConsensusAll
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local", "eu"})
	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	// Только регион "local" тикает — "eu" пока не сообщил ничего. С
	// consensus=all и корректным RegionCount=2 инцидент открыться не должен.
	localWD := fastWatchdog(svc, d, notifier)
	localWD.Region = "local"

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go localWD.Run(wctx)

	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})
	// Регион "local" уже отчитался down; "eu" ещё нет — только один из двух
	// настроенных регионов определился. С consensus=all этого недостаточно.
	assertNoOpenIncident(t, ctx, svc, created.ID)
	wcancel()

	// Второй регион "eu" тоже тикает: теперь оба региона определились и
	// оба down — consensus=all должен открыть инцидент.
	euWD := fastWatchdog(svc, d, notifier)
	euWD.Region = "eu"
	wctx2, wcancel2 := context.WithCancel(ctx)
	defer wcancel2()
	go euWD.Run(wctx2)

	waitForRunner(t, func() bool {
		_, open, err := svc.OpenIncidentFor(context.Background(), created.ID)
		return err == nil && open
	})
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
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	// Pin daysLeft deterministically to 5: ceil((expires-now)/24h) == 5 for
	// anything in (4d, 5d] from now.
	expires := time.Now().UTC().Add(4*24*time.Hour + 12*time.Hour)
	if err := svc.SetSSLExpiry(ctx, created.ID, expires); err != nil {
		t.Fatalf("SetSSLExpiry: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
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
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
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
	createdSSL := mustCreateMonitor(t, pool, svc, ctx, sslMon, []string{"local"})
	expires := time.Now().UTC().Add(4*24*time.Hour + 12*time.Hour) // daysLeft == 5
	if err := svc.SetSSLExpiry(ctx, createdSSL.ID, expires); err != nil {
		t.Fatalf("SetSSLExpiry: %v", err)
	}

	// Reminder side: an open incident already due for a reminder.
	remMon := baseHTTPMonitor(pid)
	remMon.FailThreshold = 1
	remMon.RemindEveryMinutes = 10
	remMon.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	createdRem := mustCreateMonitor(t, pool, svc, ctx, remMon, []string{"local"})
	d := &uptime.Detector{Svc: svc, Notifier: nil}
	applyAndDetect(t, ctx, svc, d, createdRem, "local", false, "boom", time.Now().UTC(), nil)
	assertOpenIncident(t, ctx, svc, createdRem.ID)
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 minutes' WHERE monitor_id = $1 AND resolved_at IS NULL",
		createdRem.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}

	// Позитивный контроль: heartbeat-монитор с протухшим ударом. Он не требует
	// Notifier — инцидент открывает Detector, — поэтому годится маркером «тик
	// состоялся» именно в этом тесте. Без него утверждения ниже («ничего не
	// помечено доставленным») остались бы зелёными и в случае, когда watchdog не
	// тикнул ни разу.
	beatMon := mustCreateMonitor(t, pool, svc, ctx, baseHeartbeatMonitor(t, pid, 1, 60), []string{"local"})
	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", beatMon.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	// wd.Notifier is deliberately nil — the field's zero value, matching the
	// "incidents only" deployment mode.
	wd := fastWatchdog(svc, d, nil)
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)
	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), beatMon.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})
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

// TestWatchdogHeartbeatMissRecordsCheckResult фиксирует P0: пропущенный удар
// обязан попадать в check_results. Успешный пинг строку пишет (web/heartbeat.go),
// поэтому без записи промаха в знаменателе аптайма остаются ОДНИ УСПЕХИ и доля
// heartbeat-монитора никогда не опускается ниже 100% — на той же странице, где
// горит "down" и висит открытый инцидент.
func TestWatchdogHeartbeatMissRecordsCheckResult(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHeartbeatMonitor(t, pid, 1, 60)
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", created.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

	writer := uptime.NewResultWriter(ch)
	go writer.Run()

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	wd := fastWatchdog(svc, d, notifier)
	wd.Writer = writer

	wctx, wcancel := context.WithCancel(ctx)
	go wd.Run(wctx)

	// Ждём, пока сторож переведёт монитор в down (значит промах обработан).
	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})
	wcancel()

	cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer ccancel()
	if err := writer.Close(cctx); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	q := uptime.NewQuery(ch)
	now := time.Now().UTC()
	stat, err := q.Uptime(cctx, created.ID, now.Add(-time.Hour), now.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("Uptime: %v", err)
	}
	if stat.Total == 0 {
		t.Fatal("промах heartbeat не записан в check_results: Total=0, доля аптайма осталась бы 100%")
	}
	if stat.OK != 0 {
		t.Fatalf("промах записан как успешный: OK=%d из Total=%d", stat.OK, stat.Total)
	}
}

// TestWatchdogPublishesTickLiveness — self-метрики живости прохода
// heartbeat+reminder: без них умерший или отставший Watchdog снаружи
// неотличим от «пропущенных heartbeat и созревших напоминаний сейчас нет».
// Мониторов и инцидентов не заводим — оба запроса тика пусты, но обязаны
// УСПЕШНО завершаться.
func TestWatchdogPublishesTickLiveness(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)

	w := &uptime.Watchdog{
		Svc: svc, Region: "local",
		Interval: 20 * time.Millisecond,
		SSLEvery: time.Hour, // не мешаем: первый прогон checkSSL идёт сразу в Run
	}
	if got := w.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := time.Now().Unix()
	go w.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for w.LastTickUnix() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := w.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := w.LastTickSeconds(); got <= 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

// TestWatchdogTickBudgetAbortsHungTick — повисший Svc (недоступный
// PostgreSQL) не должен блокировать проход heartbeat+reminder дольше
// tickBudget: тот же контракт, что host.Evaluator/metric.Evaluator.
// Notifier нарочно не задан: checkSSL (первый прогон, вне бюджета — см. его
// докблок) с нулевым Notifier выходит сразу же, не трогая Svc вовсе, иначе
// Run завис бы на SSLCandidates ещё ДО первого тика.
func TestWatchdogTickBudgetAbortsHungTick(t *testing.T) {
	svc := uptime.NewService(blackholePool(t))
	w := &uptime.Watchdog{Svc: svc, Region: "local", Interval: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	deadline := time.Now().Add(60 * time.Second)
	for w.LastTickSeconds() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := w.LastTickSeconds(); got <= 0 {
		t.Fatal("проход heartbeat+reminder не завершился за 60с: повисший PostgreSQL блокирует Watchdog")
	}
	if got := w.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по бюджету прохода, want 0", got)
	}
}

// syncBuf — mutex-guarded log sink, safe to poll from the test goroutine
// while slog writes concurrently from the Watchdog's own goroutine (a plain
// bytes.Buffer would race under `go test -race`).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWatchdogSSLCheckBudgetAbortsHungCheck — повисший checkSSL (недоступный
// PostgreSQL — SSLCandidates без своего таймаута) не должен блокировать
// проверку сертификатов дольше sslCheckBudget (ревью W3-D, финальная
// находка): без бюджета первый же безусловный прогон checkSSL в Run вешал бы
// Watchdog целиком на самом старте процесса, ещё до первого
// heartbeat/reminder-тика. checkSSL не публикует свою self-метрику
// (суточный горизонт нельзя валидно смешивать с минутным LastTickUnix/
// LastTickSeconds — см. докблок sslCheckBudget), поэтому наблюдаем через
// лог: budget истёк → SSLCandidates вернула context.deadlineExceeded →
// slog.Error с "ssl candidates failed".
func TestWatchdogSSLCheckBudgetAbortsHungCheck(t *testing.T) {
	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	svc := uptime.NewService(blackholePool(t))
	w := &uptime.Watchdog{Svc: svc, Notifier: &fakeNotifier{}, Region: "local"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Run зовёт checkSSL безусловно ДО входа в тикер (см. её докблок) — сам
	// факт вызова Run уже запускает первый (и единственный нужный тесту)
	// прогон.
	go w.Run(ctx)

	deadline := time.Now().Add(60 * time.Second)
	for !strings.Contains(logs.String(), "ssl candidates failed") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	out := logs.String()
	if !strings.Contains(out, "ssl candidates failed") {
		t.Fatal("checkSSL не завершился за 60с: повисший PostgreSQL блокирует проверку сертификатов")
	}
	if !strings.Contains(out, "context deadline exceeded") {
		t.Errorf("лог не называет причиной истечение бюджета (context deadline exceeded):\n%s", out)
	}
}

// reminderMonitor — монитор с напоминаниями раз в 10 минут и уже открытым,
// доставленным (notified_open) инцидентом, отодвинутым на 30 минут назад:
// напоминание по нему созрело сразу. Общая заготовка тестов напоминаний.
func reminderMonitor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, svc *uptime.Service, d *uptime.Detector, pid int64) uptime.Monitor {
	t.Helper()
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 1
	m.RemindEveryMinutes = 10
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})
	applyAndDetect(t, ctx, svc, d, created, "local", false, "boom", time.Now().UTC(), nil)
	inc := assertOpenIncident(t, ctx, svc, created.ID)
	if inc.InMaintenance {
		t.Fatalf("incident opened with in_maintenance=true, want the snapshot to say false (no window at open time)")
	}
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 minutes' WHERE monitor_id = $1 AND resolved_at IS NULL",
		created.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}
	return created
}

// dueReminderIDs — идентификаторы инцидентов, которые IncidentsDueForReminder
// считает созревшими прямо сейчас.
func dueReminderIDs(t *testing.T, ctx context.Context, svc *uptime.Service) map[int64]bool {
	t.Helper()
	items, err := svc.IncidentsDueForReminder(ctx)
	if err != nil {
		t.Fatalf("IncidentsDueForReminder: %v", err)
	}
	ids := map[int64]bool{}
	for _, it := range items {
		ids[it.Incident.ID] = true
	}
	return ids
}

// TestRemindersSkippedDuringMaintenanceWindow (K2-1): окно обслуживания,
// начавшееся ПОСЛЕ открытия инцидента, глушит напоминания живой проверкой
// Watchdog.Maint — снимок in_maintenance=false этого окна не видит. Пока окно
// идёт, напоминание не уходит и last_reminded_at не двигается (клейма нет);
// как только окно снято, напоминание уходит первым же тиком. Два монитора в
// одном проекте — чтобы проход прошёл и через живой вызов InMaintenance, и
// через кэш решения на тик.
func TestRemindersSkippedDuringMaintenanceWindow(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	first := reminderMonitor(t, ctx, pool, svc, d, pid)
	second := reminderMonitor(t, ctx, pool, svc, d, pid)

	// Окно начинается после открытия инцидента: снимок остаётся false.
	start := time.Now().UTC().Add(-time.Minute)
	end := time.Now().UTC().Add(time.Hour)
	w, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid, Name: "late window", StartsAt: &start, EndsAt: &end, Timezone: "UTC",
	})
	if err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	wd := fastWatchdog(svc, d, notifier)
	wd.Maint = svc
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	// Дождаться хотя бы одного завершённого прохода и дать ещё несколько.
	waitForRunner(t, func() bool { return wd.LastTickUnix() != 0 })
	time.Sleep(150 * time.Millisecond)

	if got := notifier.kindEvents("reminder"); len(got) != 0 {
		t.Fatalf("reminder events during maintenance window = %d, want 0 (live window check must skip them)", len(got))
	}
	for _, mon := range []uptime.Monitor{first, second} {
		inc := assertOpenIncident(t, ctx, svc, mon.ID)
		if inc.LastRemindedAt != nil {
			t.Fatalf("monitor %d: LastRemindedAt = %v during maintenance window, want nil (no claim while skipped)", mon.ID, inc.LastRemindedAt)
		}
	}

	// Окно снято — напоминания уходят следующим тиком, по одному на инцидент.
	if err := svc.DeleteWindow(ctx, w.ID, pid); err != nil {
		t.Fatalf("DeleteWindow: %v", err)
	}
	waitForRunner(t, func() bool { return len(notifier.kindEvents("reminder")) >= 2 })
	wcancel()

	reminders := notifier.kindEvents("reminder")
	if len(reminders) != 2 {
		t.Fatalf("reminder events after window = %d, want 2: %+v", len(reminders), reminders)
	}
	for _, mon := range []uptime.Monitor{first, second} {
		inc := assertOpenIncident(t, ctx, svc, mon.ID)
		if inc.LastRemindedAt == nil {
			t.Fatalf("monitor %d: LastRemindedAt is nil after window ended, want it set", mon.ID)
		}
	}
}

// failingMaint — проверка окна, которая всегда падает.
type failingMaint struct{}

func (failingMaint) InMaintenance(context.Context, int64, time.Time) (bool, error) {
	return false, errors.New("windows unavailable")
}

// TestRemindersSentWhenMaintenanceCheckFails (K2-1): ошибка живой проверки
// окна НЕ глушит напоминание — отказ падает в сторону оповещения, не тишины
// (см. докблок checkReminders), и пишется в журнал предупреждением.
func TestRemindersSentWhenMaintenanceCheckFails(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	mon := reminderMonitor(t, ctx, pool, svc, d, pid)

	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)

	wd := fastWatchdog(svc, d, notifier)
	wd.Maint = failingMaint{}
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go wd.Run(wctx)

	waitForRunner(t, func() bool { return len(notifier.kindEvents("reminder")) >= 1 })
	wcancel()

	if inc := assertOpenIncident(t, ctx, svc, mon.ID); inc.LastRemindedAt == nil {
		t.Fatalf("LastRemindedAt is nil, want the reminder claimed despite the failing maintenance check")
	}
	if !strings.Contains(logs.String(), "maintenance check failed") {
		t.Fatalf("log = %q, want a warning about the failed maintenance check", logs.String())
	}
}

// TestRemindersSkippedForDisabledMonitor (K2-2): монитор на паузе не даёт
// напоминаний по своему открытому инциденту, но инцидент при этом НЕ
// закрывается; снятие паузы возвращает напоминание в выдачу.
func TestRemindersSkippedForDisabledMonitor(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	mon := reminderMonitor(t, ctx, pool, svc, d, pid)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)

	if !dueReminderIDs(t, ctx, svc)[inc.ID] {
		t.Fatalf("incident %d not due for reminder while monitor enabled, want it listed", inc.ID)
	}

	if err := svc.SetEnabled(ctx, mon.ID, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if dueReminderIDs(t, ctx, svc)[inc.ID] {
		t.Fatalf("incident %d due for reminder while monitor disabled, want it skipped", inc.ID)
	}
	if still := assertOpenIncident(t, ctx, svc, mon.ID); still.ID != inc.ID {
		t.Fatalf("open incident after pause = %d, want the same %d (pause must not resolve it)", still.ID, inc.ID)
	}

	if err := svc.SetEnabled(ctx, mon.ID, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !dueReminderIDs(t, ctx, svc)[inc.ID] {
		t.Fatalf("incident %d not due for reminder after unpause, want it listed again", inc.ID)
	}
}
