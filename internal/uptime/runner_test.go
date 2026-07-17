package uptime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// waitForRunner polls cond until it's true or 5s pass, failing the test on
// timeout — the runner's tickers are fast in these tests (tens of ms) but
// still async, so a hard assertion right after starting it would flake.
func waitForRunner(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in 5s")
}

// newFastRunner builds a Runner with tickers fast enough for tests, backed
// by real PG+CH.
func newFastRunner(svc *uptime.Service, writer *uptime.ResultWriter) *uptime.Runner {
	return &uptime.Runner{
		Svc:           svc,
		Writer:        writer,
		Region:        "local",
		Concurrency:   5,
		ScheduleEvery: 20 * time.Millisecond,
		LeaseEvery:    20 * time.Millisecond,
		// Тесты мониторят loopback-серверы httptest — отключаем SSRF-фильтр
		// приватных целей, иначе проверки резались бы до соединения.
		AllowPrivateTargets: true,
	}
}

func TestRunnerChecksSchedulesLeasesAndCompletesJob(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.IntervalSeconds = 30
	m.TimeoutSeconds = 5
	m.RecoveryThreshold = 1 // одного успеха достаточно, чтобы увидеть status=up
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: srv.URL})
	created := mustCreateMonitor(t, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})

	// Poll on the state landing as "up" — polling PendingCount alone would
	// race: it reads 0 both before anything has been scheduled yet and
	// after the job has been fully processed, so it can spuriously look
	// "done" on the very first poll.
	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "up"
	})

	states, err := svc.States(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("States() = %d entries, want 1", len(states))
	}
	if states[0].Status != "up" {
		t.Fatalf("state.Status = %q, want up", states[0].Status)
	}
	if states[0].Region != "local" {
		t.Fatalf("state.Region = %q, want local", states[0].Region)
	}

	waitForRunner(t, func() bool {
		n, err := svc.PendingCount(context.Background())
		return err == nil && n == 0
	})

	cancel()
	runner.Close()

	// Flush the writer's buffer synchronously before reading it back — it
	// only ever ticks every 5s (interval default) or at 1000 buffered rows
	// (see ResultWriter.NewResultWriter), neither of which this test hits.
	chCtx, chCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer chCancel()
	if err := writer.Close(chCtx); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	var cnt uint64
	if err := ch.QueryRow(chCtx,
		"SELECT count(*) FROM check_results WHERE monitor_id = $1", uint64(created.ID)).Scan(&cnt); err != nil {
		t.Fatalf("count check_results: %v", err)
	}
	if cnt == 0 {
		t.Fatalf("check_results count = 0, want >= 1")
	}

	var ok uint8
	if err := ch.QueryRow(chCtx,
		"SELECT ok FROM check_results WHERE monitor_id = $1 LIMIT 1", uint64(created.ID)).Scan(&ok); err != nil {
		t.Fatalf("select ok: %v", err)
	}
	if ok != 1 {
		t.Fatalf("ok = %d, want 1", ok)
	}
}

func TestRunnerRecordsFailureAndStillCompletesJob(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.IntervalSeconds = 30
	m.TimeoutSeconds = 5
	m.FailThreshold = 1 // одной ошибки достаточно, чтобы увидеть status=down
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: srv.URL})
	created := mustCreateMonitor(t, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})

	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})

	states, err := svc.States(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "down" {
		t.Fatalf("states = %+v, want single down state", states)
	}
	if states[0].LastError == "" {
		t.Fatalf("LastError is empty, want a checker error message")
	}

	waitForRunner(t, func() bool {
		n, err := svc.PendingCount(context.Background())
		return err == nil && n == 0
	})
}

func TestRunnerInvokesOnResultCallback(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.IntervalSeconds = 30
	m.TimeoutSeconds = 5
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: srv.URL})
	created := mustCreateMonitor(t, svc, context.Background(), m, []string{"local"})

	type call struct {
		monitorID int64
		region    string
		ok        bool
	}
	calls := make(chan call, 1)

	runner := newFastRunner(svc, writer)
	runner.OnResult = func(_ context.Context, mon uptime.Monitor, region string, r uptime.Result, _ uptime.State) {
		calls <- call{monitorID: mon.ID, region: region, ok: r.OK}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})

	select {
	case c := <-calls:
		if c.monitorID != created.ID || c.region != "local" || !c.ok {
			t.Fatalf("OnResult call = %+v, want monitor %d region local ok=true", c, created.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnResult was not called within 5s")
	}

	// OnResult fires before CompleteJob (see runOne) — give the same
	// goroutine a moment to reach CompleteJob before t.Cleanup cancels ctx,
	// so the cleanup doesn't race a legitimate in-flight DB call.
	waitForRunner(t, func() bool {
		n, err := svc.PendingCount(context.Background())
		return err == nil && n == 0
	})
}

// TestRunnerCloseWaitsForInFlightCheck verifies that Close() blocks until a
// check already in flight when it's called has finished and completed its
// job — not just until the ticker loop stops — AND that the result of that
// check actually gets persisted. This mirrors the REAL production ordering
// (cmd/gotcha/main.go's drain()): the run ctx is cancelled first
// (signal.NotifyContext firing / shutdown), and only THEN is Close() called.
//
// Cancelling ctx aborts the in-flight HTTP round-trip immediately (the
// checker uses ctx via http.NewRequestWithContext) — that's intentional, a
// shutdown should abort a slow check quickly rather than wait it out. But
// the resulting Result{OK:false} must still make it to ApplyResult and
// CompleteJob: if runOne's post-check DB writes used that same
// already-cancelled ctx, they'd ALSO fail with "context canceled" and the
// result would be silently dropped (PendingCount stuck at 1, monitor_state
// never updated) even though Close() dutifully blocked for it.
func TestRunnerCloseWaitsForInFlightCheck(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.IntervalSeconds = 30
	m.TimeoutSeconds = 5
	m.FailThreshold = 1 // one failed (canceled) check is enough to see status=down
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: srv.URL})
	created := mustCreateMonitor(t, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("check did not start within 5s")
	}

	// Real shutdown ordering: cancel the run ctx FIRST (as main.go's
	// signal.NotifyContext does), THEN call Close(). The check is already
	// in flight when ctx is cancelled, so it aborts fast (context canceled)
	// instead of waiting out the server's 200ms sleep — what's under test is
	// whether the POST-check DB writes for that aborted check still survive
	// the cancellation.
	cancel()
	runner.Close() // must block until the in-flight check (and its CompleteJob) finishes

	pending, err := svc.PendingCount(context.Background())
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d after Close, want 0 (in-flight check must have completed)", pending)
	}

	states, err := svc.States(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "down" {
		t.Fatalf("states = %+v, want single down state (result must have been persisted despite ctx cancellation)", states)
	}
	if states[0].LastError == "" {
		t.Fatalf("LastError is empty, want the canceled-context error message")
	}
}

// panicChecker always panics — used to verify runOne recovers from a
// checker panic instead of taking the whole process down with it.
type panicChecker struct{}

func (panicChecker) Check(ctx context.Context, m uptime.Monitor) uptime.Result {
	panic("boom: checker bug")
}

// TestRunnerRecoversFromCheckerPanic verifies that a panicking Checker
// doesn't take down the runner (and, in --mode=all, the whole process): the
// panic must be recovered and turned into a failed Result, with the job
// still completed and the failure visible in monitor_state.
func TestRunnerRecoversFromCheckerPanic(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.IntervalSeconds = 30
	m.TimeoutSeconds = 5
	m.FailThreshold = 1
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "http://127.0.0.1:1"})
	created := mustCreateMonitor(t, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	runner.Checkers = map[uptime.Kind]uptime.Checker{uptime.KindHTTP: panicChecker{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})
	go runner.Run(ctx)

	// The runner (this goroutine, in particular) must survive the panic:
	// polling PendingCount / States below would just hang/timeout if the
	// panic had propagated and killed the runner's worker goroutine pool
	// (it wouldn't take the test process down since recover() only stops
	// unwinding in the panicking goroutine, but the job would never
	// complete).
	waitForRunner(t, func() bool {
		states, err := svc.States(context.Background(), created.ID)
		return err == nil && len(states) == 1 && states[0].Status == "down"
	})

	states, err := svc.States(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "down" {
		t.Fatalf("states = %+v, want single down state", states)
	}
	if states[0].LastError == "" {
		t.Fatalf("LastError is empty, want a recovered-panic error message")
	}

	waitForRunner(t, func() bool {
		n, err := svc.PendingCount(context.Background())
		return err == nil && n == 0
	})
}
