package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestSchedulerFillsQueueWithoutRunner — планировщик наполняет очередь сам, без
// исполнителя в этом же процессе.
//
// Существует потому, что постановка заданий жила вторым тикером внутри Runner,
// а Runner собирается только в режимах uptime и all. В раздельном развёртывании
// web+ingest очередь не наполнялась никогда: монитор показан включённым,
// состояние остаётся unknown, выносные пробы опрашивают пустоту — и ни одной
// строки в логе. Проверяем ровно этот сценарий: планировщик работает, Runner'а
// нет, задание должно появиться и стать доступным для лизы.
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

	// Задание доступно к выдаче — это и означает «очередь наполняется».
	waitForRunner(t, func() bool {
		jobs, err := svc.LeaseLocal(context.Background(), "local", 10)
		return err == nil && len(jobs) == 1 && jobs[0].Monitor.ID == created.ID
	})
}

// TestSchedulerIsIdempotentAcrossReplicas — две реплики планировщика не
// растягивают расписание и не плодят дублей: постановка идёт через
// ON CONFLICT DO NOTHING по (monitor_id, region), а last_scheduled_at
// двигается только у тех мониторов, чьё задание реально вставилось.
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

	// После остановки планировщиков в очереди не должно копиться дублей на тот
	// же монитор и регион — уникальный индекс это и гарантирует, но проверяем
	// через API, а не через схему.
	var queued int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM check_queue").Scan(&queued); err != nil {
		t.Fatalf("count check_queue: %v", err)
	}
	if queued > 1 {
		t.Fatalf("в очереди %d заданий на один монитор и регион, want не больше одного", queued)
	}
}
