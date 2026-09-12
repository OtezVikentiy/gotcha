package uptime_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// pgxpool.Pool — конкретный тип, не интерфейс; вместо фейка тут слушатель,
// который принимает соединение и никогда не отвечает на handshake.
func blackholePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	// соединения хранить обязательно: брошенный сокет закрывает GC-финализатор,
	// и RST на непрочитанный стартовый пакет превращает заглушку в мгновенный отказ.
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	dsn := fmt.Sprintf("postgres://nobody:nobody@%s/none?sslmode=disable", ln.Addr().String())
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

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

func newFastRunner(svc *uptime.Service, writer *uptime.ResultWriter) *uptime.Runner {
	return &uptime.Runner{
		Svc:         svc,
		Writer:      writer,
		Region:      "local",
		Concurrency: 5,
		LeaseEvery:  20 * time.Millisecond,
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
	created := mustCreateMonitor(t, pool, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	// Постановка заданий вынесена из Runner в Scheduler — запускаем его рядом,
	// иначе очередь останется пустой и лизить будет нечего.
	go (&uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}).Run(ctx)
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})

	// PendingCount один не годится: оно 0 и до постановки, и после обработки —
	// первый же опрос может ошибочно решить, что всё готово.
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

	// естественный тик (5с) или 1000 строк тест не набирает — флашим синхронно.
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
	created := mustCreateMonitor(t, pool, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	// Постановка заданий вынесена из Runner в Scheduler — запускаем его рядом,
	// иначе очередь останется пустой и лизить будет нечего.
	go (&uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}).Run(ctx)
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
	created := mustCreateMonitor(t, pool, svc, context.Background(), m, []string{"local"})

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
	// Постановка заданий вынесена из Runner в Scheduler — запускаем его рядом,
	// иначе очередь останется пустой и лизить будет нечего.
	go (&uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}).Run(ctx)
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

	// OnResult срабатывает раньше CompleteJob — ждём, чтобы cleanup не оборвал
	// ещё идущий DB-вызов гонкой отмены ctx.
	waitForRunner(t, func() bool {
		n, err := svc.PendingCount(context.Background())
		return err == nil && n == 0
	})
}

// повторяет прод-порядок shutdown: сначала отменяется ctx (main.go drain()),
// потом Close — и оборванная этим проверка не должна выглядеть как авария.
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
	created := mustCreateMonitor(t, pool, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	ctx, cancel := context.WithCancel(context.Background())
	go runner.Run(ctx)
	// Постановка заданий вынесена из Runner в Scheduler — запускаем его рядом,
	// иначе очередь останется пустой и лизить будет нечего.
	go (&uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}).Run(ctx)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("check did not start within 5s")
	}

	cancel()
	runner.Close() // must block until the in-flight check finishes

	// результат отброшен — проверку перевыполнит другая реплика или этот же
	// процесс после рестарта, когда истечёт лиза.
	pending, err := svc.PendingCount(context.Background())
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 1 {
		t.Fatalf("PendingCount() = %d after Close, want 1 (aborted check must be redone, not lost)", pending)
	}

	states, err := svc.States(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states = %+v, want none: shutdown must not look like an outage", states)
	}
}

type panicChecker struct{}

func (panicChecker) Check(ctx context.Context, m uptime.Monitor) uptime.Result {
	panic("boom: checker bug")
}

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
	created := mustCreateMonitor(t, pool, svc, context.Background(), m, []string{"local"})

	runner := newFastRunner(svc, writer)
	runner.Checkers = map[uptime.Kind]uptime.Checker{uptime.KindHTTP: panicChecker{}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})
	go runner.Run(ctx)
	// Постановка заданий вынесена из Runner в Scheduler — запускаем его рядом,
	// иначе очередь останется пустой и лизить будет нечего.
	go (&uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}).Run(ctx)

	// если паника всё же убьёт воркер, тест не упадёт от паники (recover не
	// распространяется), а зависнет на поллинге — так и опознаётся регресс.
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

// мониторов не заводим — лизинг пуст, но обязан УСПЕШНО завершаться.
func TestRunnerPublishesTickLiveness(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)

	runner := &uptime.Runner{Svc: svc, Region: "local", LeaseEvery: 20 * time.Millisecond}
	if got := runner.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	before := time.Now().Unix()
	go runner.Run(ctx)
	t.Cleanup(func() {
		cancel()
		runner.Close()
	})

	waitForRunner(t, func() bool { return runner.LastTickUnix() != 0 })
	if got := runner.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := runner.LastTickSeconds(); got <= 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

// раздача по семафору исполнителям в бюджет не входит, лизинг PG — входит.
func TestRunnerLeaseBudgetAbortsHungLease(t *testing.T) {
	svc := uptime.NewService(blackholePool(t))
	runner := &uptime.Runner{Svc: svc, Region: "local", LeaseEvery: 20 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runner.Run(ctx)
	t.Cleanup(runner.Close)

	deadline := time.Now().Add(60 * time.Second)
	for runner.LastTickSeconds() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runner.LastTickSeconds(); got <= 0 {
		t.Fatal("leaseAndDispatch не завершился за 60с: повисший PostgreSQL блокирует лизинг")
	}
	if got := runner.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по бюджету лизинга, want 0", got)
	}
}
