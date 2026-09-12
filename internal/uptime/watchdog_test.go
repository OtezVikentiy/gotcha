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
	// fail_threshold=1 — одного пропущенного тика достаточно, чтобы дойти до down.
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

	// ждём именно инцидента: состояние пишется на шаг раньше его открытия,
	// и ожидание по состоянию ловит окно между двумя записями.
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

	// позитивный контроль — второй монитор с протухшим ударом: без него тест
	// зеленел бы даже если watchdog не тикнул ни разу.
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
	// только local тикает — с consensus=all и RegionCount=2 инцидент открыться не должен.
	localWD := fastWatchdog(svc, d, notifier)
	localWD.Region = "local"

	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go localWD.Run(wctx)

	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})
	assertNoOpenIncident(t, ctx, svc, created.ID)
	wcancel()

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

	// daysLeft детерминированно 5: ceil((expires-now)/24ч) = 5 для (4д,5д].
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
	// даём ещё нескольким быстрым тикам шанс (ошибочно) сработать повторно.
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
	// daysLeft=5 пересекает и 14 (ssl_alert_days), и встроенный 7 разом —
	// оба фиксируются одним Notify, иначе следующий тик переалертил бы 7.
	if !alertedSet[14] || !alertedSet[7] {
		t.Fatalf("ssl_alerted_days = %v, want it to contain 14 and 7", alerted)
	}

	wcancel()

	// имитируем сутки спустя: daysLeft падает до 3, пересекая встроенный порог 3.
	expires3 := time.Now().UTC().Add(2*24*time.Hour + 12*time.Hour)
	if err := svc.SetSSLExpiry(ctx, created.ID, expires3); err != nil {
		t.Fatalf("SetSSLExpiry (day later): %v", err)
	}
	// expiry earlier, не later — ssl_alerted_days не обнуляется, {14,7} остаются.
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

	// инцидент состарен на 30 минут — при remind_every=10 напоминание уже просрочено.
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
	// ещё тики: last_reminded_at теперь "сейчас", второе напоминание не должно прийти долго.
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

func TestWatchdogNilNotifierDoesNotMarkDelivered(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)

	sslMon := baseHTTPMonitor(pid)
	sslMon.SSLAlertDays = 14
	sslMon.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	createdSSL := mustCreateMonitor(t, pool, svc, ctx, sslMon, []string{"local"})
	expires := time.Now().UTC().Add(4*24*time.Hour + 12*time.Hour) // daysLeft == 5
	if err := svc.SetSSLExpiry(ctx, createdSSL.ID, expires); err != nil {
		t.Fatalf("SetSSLExpiry: %v", err)
	}

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

	// позитивный контроль: heartbeat не требует Notifier — маркер «тик
	// состоялся» в этом тесте, иначе assertions ниже были бы пусто зелёными.
	beatMon := mustCreateMonitor(t, pool, svc, ctx, baseHeartbeatMonitor(t, pid, 1, 60), []string{"local"})
	if _, err := pool.Exec(ctx,
		"UPDATE monitors SET last_beat_at = now() - interval '5 minutes' WHERE id = $1", beatMon.ID); err != nil {
		t.Fatalf("backdate last_beat_at: %v", err)
	}

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

// мониторов и инцидентов нет — оба запроса тика пусты, но обязаны успешно завершаться.
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

// Notifier не задан: checkSSL пропускает claim/Notify целиком (см. её
// нулевой-Notifier ветку) и не трогает Svc — иначе завис бы ещё до первого тика.
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

// mutex вокруг bytes.Buffer — иначе гонка под -race: slog пишет из горутины
// Watchdog, тест читает из своей.
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

// checkSSL не публикует свою self-метрику (суточный горизонт с минутным
// LastTick* не смешать) — наблюдаем по логу: budget истёк → slog.Error.
func TestWatchdogSSLCheckBudgetAbortsHungCheck(t *testing.T) {
	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	svc := uptime.NewService(blackholePool(t))
	w := &uptime.Watchdog{Svc: svc, Notifier: &fakeNotifier{}, Region: "local"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Run зовёт checkSSL безусловно до тикера — сам вызов Run уже даёт нужный прогон.
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

// напоминание раз в 10 минут, инцидент открыт, notified_open, отодвинут на
// 30 минут назад — напоминание по нему созрело сразу.
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

// окно, начавшееся после открытия инцидента, снимок in_maintenance=false не видит —
// напоминание уйдёт первым тиком после снятия окна; два монитора кроют live+кэш.
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

type failingMaint struct{}

func (failingMaint) InMaintenance(context.Context, int64, time.Time) (bool, error) {
	return false, errors.New("windows unavailable")
}

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
