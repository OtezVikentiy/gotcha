package uptime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// leaseOneJob schedules everything due and leases a single local job — the
// starting point of every Ingestor test (Accept works on an already-leased
// Job, exactly like Runner.runOne and /probe/results do).
func leaseOneJob(t *testing.T, ctx context.Context, svc *uptime.Service) uptime.Job {
	t.Helper()
	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	jobs, err := svc.LeaseLocal(ctx, "local", 10)
	if err != nil {
		t.Fatalf("LeaseLocal: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("LeaseLocal() = %d jobs, want 1", len(jobs))
	}
	return jobs[0]
}

func TestIngestorAcceptWritesResultUpdatesStateAndCompletesJob(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 1
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	job := leaseOneJob(t, ctx, svc)

	type call struct {
		monitorID int64
		region    string
		ok        bool
		status    string
	}
	calls := make(chan call, 1)

	ing := &uptime.Ingestor{
		Svc:    svc,
		Writer: writer,
		OnResult: func(_ context.Context, mon uptime.Monitor, region string, r uptime.Result, st uptime.State) {
			calls <- call{monitorID: mon.ID, region: region, ok: r.OK, status: st.Status}
		},
	}

	at := time.Now().UTC()
	res := uptime.Result{OK: false, StatusCode: 500, Error: "boom", TotalMs: 9}
	if err := ing.Accept(ctx, job, at, res); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	select {
	case c := <-calls:
		if c.monitorID != created.ID || c.region != "local" || c.ok || c.status != "down" {
			t.Fatalf("OnResult call = %+v, want monitor %d region local ok=false status=down", c, created.ID)
		}
	default:
		t.Fatal("OnResult was not called")
	}

	states, err := svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "down" || states[0].LastError != "boom" {
		t.Fatalf("states = %+v, want single down state with LastError=boom", states)
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (job must be completed)", pending)
	}

	if err := writer.Close(ctx); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}
	var cnt uint64
	if err := ch.QueryRow(ctx,
		"SELECT count(*) FROM check_results WHERE monitor_id = $1 AND ok = 0", uint64(created.ID)).Scan(&cnt); err != nil {
		t.Fatalf("count check_results: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("check_results count = %d, want 1", cnt)
	}
}

// TestIngestorAcceptWithoutWriter — Writer=nil (стенд без ClickHouse):
// результат всё равно доходит до monitor_state и снимает задание с очереди,
// просто не пишется в CH.
func TestIngestorAcceptWithoutWriter(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.RecoveryThreshold = 1
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	job := leaseOneJob(t, ctx, svc)

	ing := &uptime.Ingestor{Svc: svc}
	if err := ing.Accept(ctx, job, time.Now().UTC(), uptime.Result{OK: true, StatusCode: 200}); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	states, err := svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Status != "up" {
		t.Fatalf("states = %+v, want single up state", states)
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0", pending)
	}
}

// TestIngestorAcceptAppliesAJobExactlyOnce — регрессия на гонку «одна проверка
// применена дважды». Два одновременных Accept с ОДНИМ И ТЕМ ЖЕ заданием (два
// параллельных POST /probe/results с одинаковым queue_id — тот же токен пробы,
// запущенный в двух процессах; либо две реплики, из которых одна перехватила
// протухший lease другой) обязаны дать ровно один эффект: одну строку в
// ClickHouse, consecutive_fails == 1 и один вызов детектора. До claim-first в
// Accept оба проходили LeasedJob, оба звали ApplyResult (атомарный, но НЕ
// идемпотентный: consecutive_fails + 1), и один упавший чек с fail_threshold=2
// клал монитор.
func TestIngestorAcceptAppliesAJobExactlyOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := uptime.NewService(pool)
	writer := uptime.NewResultWriter(ch)
	go writer.Run()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 2 // ровно тот порог, при котором двойной учёт кладёт монитор
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

	job := leaseOneJob(t, ctx, svc)

	var mu sync.Mutex
	var onResultCalls int
	ing := &uptime.Ingestor{
		Svc:    svc,
		Writer: writer,
		OnResult: func(_ context.Context, _ uptime.Monitor, _ string, _ uptime.Result, _ uptime.State) {
			mu.Lock()
			onResultCalls++
			mu.Unlock()
		},
	}

	// Оба вызывающих видят одно и то же живое задание (каждый успел бы получить
	// его из LeasedJob) и применяют его результат одновременно.
	res := uptime.Result{OK: false, StatusCode: 500, Error: "boom", TotalMs: 9}
	at := time.Now().UTC()
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = ing.Accept(ctx, job, at, res)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Accept #%d: %v", i, err)
		}
	}

	if onResultCalls != 1 {
		t.Errorf("OnResult called %d times, want 1 — the detector must see one check once", onResultCalls)
	}

	states, err := svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("States() = %+v, want a single state", states)
	}
	if states[0].ConsecutiveFails != 1 {
		t.Errorf("ConsecutiveFails = %d, want 1 — one failed check must count once", states[0].ConsecutiveFails)
	}
	if states[0].Status == "down" {
		t.Errorf("Status = down after ONE failed check with fail_threshold=2 — the check was applied twice")
	}

	pending, err := svc.PendingCount(ctx)
	if err != nil {
		t.Fatalf("PendingCount: %v", err)
	}
	if pending != 0 {
		t.Fatalf("PendingCount() = %d, want 0 (the claim completes the job)", pending)
	}

	if err := writer.Close(ctx); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}
	var cnt uint64
	if err := ch.QueryRow(ctx,
		"SELECT count(*) FROM check_results WHERE monitor_id = $1", uint64(created.ID)).Scan(&cnt); err != nil {
		t.Fatalf("count check_results: %v", err)
	}
	if cnt != 1 {
		t.Errorf("check_results count = %d, want 1 — one check must produce one row", cnt)
	}
}

func TestLeasedJobOnlyForOwningProbeAndLiveLease(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	orgID := newOrgID(t, pool)
	probe, _, err := svc.CreateProbe(ctx, orgID, "eu-west", "Probe 1")
	if err != nil {
		t.Fatalf("CreateProbe: %v", err)
	}
	other, _, err := svc.CreateProbe(ctx, orgID, "eu-west", "Probe 2")
	if err != nil {
		t.Fatalf("CreateProbe (other): %v", err)
	}

	pid := newProjectInOrg(t, pool, orgID)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"eu-west"})

	if _, err := svc.Schedule(ctx); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	jobs, err := svc.LeaseForProbe(ctx, probe, 10)
	if err != nil {
		t.Fatalf("LeaseForProbe: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("LeaseForProbe() = %d jobs, want 1", len(jobs))
	}
	queueID := jobs[0].QueueID

	got, err := svc.LeasedJob(ctx, queueID, probe.ID)
	if err != nil {
		t.Fatalf("LeasedJob: %v", err)
	}
	if got.QueueID != queueID || got.MonitorID != created.ID || got.Region != "eu-west" {
		t.Fatalf("LeasedJob() = %+v, want queue %d monitor %d region eu-west", got, queueID, created.ID)
	}
	if got.Monitor.ID != created.ID || got.Monitor.Kind != uptime.KindHTTP {
		t.Fatalf("LeasedJob().Monitor = %+v, want id=%d kind=http", got.Monitor, created.ID)
	}

	if _, err := svc.LeasedJob(ctx, queueID, other.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("LeasedJob for another probe: err = %v, want ErrNotFound", err)
	}

	// Протухший lease — задание больше не принадлежит пробе, даже своей.
	if _, err := pool.Exec(ctx, "UPDATE check_queue SET lease_until = now() - interval '1 minute' WHERE id = $1", queueID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := svc.LeasedJob(ctx, queueID, probe.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("LeasedJob after lease expiry: err = %v, want ErrNotFound", err)
	}

	// Несуществующее задание.
	if _, err := svc.LeasedJob(ctx, 999999, probe.ID); !errors.Is(err, uptime.ErrNotFound) {
		t.Fatalf("LeasedJob for unknown queue id: err = %v, want ErrNotFound", err)
	}
}
