package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestSchedulerFillsQueueWithoutRunner(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	sctx, scancel := context.WithCancel(ctx)
	defer scancel()
	go (&uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}).Run(sctx)

	waitForRunner(t, func() bool {
		jobs, err := svc.LeaseLocal(context.Background(), "local", 10)
		return err == nil && len(jobs) == 1 && jobs[0].Monitor.ID == created.ID
	})
}

func TestSchedulerIsIdempotentAcrossReplicas(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	sctx, scancel := context.WithCancel(ctx)
	go (&uptime.Scheduler{Svc: svc, Every: 10 * time.Millisecond}).Run(sctx)
	go (&uptime.Scheduler{Svc: svc, Every: 10 * time.Millisecond}).Run(sctx)

	waitForRunner(t, func() bool {
		jobs, err := svc.LeaseLocal(context.Background(), "local", 10)
		return err == nil && len(jobs) >= 1
	})
	scancel()

	// уникальный индекс гарантирует это на уровне схемы; здесь то же самое
	// проверяем через публичное API.
	var queued int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM check_queue").Scan(&queued); err != nil {
		t.Fatalf("count check_queue: %v", err)
	}
	if queued > 1 {
		t.Fatalf("в очереди %d заданий на один монитор и регион, want не больше одного", queued)
	}
}

func TestSchedulerPublishesTickLiveness(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)

	sched := &uptime.Scheduler{Svc: svc, Every: 20 * time.Millisecond}
	if got := sched.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := time.Now().Unix()
	go sched.Run(ctx)

	waitForRunner(t, func() bool { return sched.LastTickUnix() != 0 })
	if got := sched.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения постановки)", got, before)
	}
	if got := sched.LastTickSeconds(); got <= 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}
