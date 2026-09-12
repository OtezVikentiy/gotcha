package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// держит проверку, пока не отменят ctx, — то же самое происходит с медленным
// HTTP-запросом в момент SIGTERM.
type blockingChecker struct{ started chan struct{} }

func (b *blockingChecker) Check(ctx context.Context, m uptime.Monitor) uptime.Result {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return uptime.Result{OK: false, Error: ctx.Err().Error()}
}

func TestShutdownDoesNotRecordFalseOutage(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 1
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, pool, svc, ctx, m, []string{"local"})

	checker := &blockingChecker{started: make(chan struct{}, 1)}
	runner := &uptime.Runner{
		Svc:                 svc,
		Region:              "local",
		LeaseEvery:          10 * time.Millisecond,
		Checkers:            map[uptime.Kind]uptime.Checker{uptime.KindHTTP: checker},
		AllowPrivateTargets: true,
	}

	runCtx, runCancel := context.WithCancel(ctx)
	go (&uptime.Scheduler{Svc: svc, Every: 10 * time.Millisecond}).Run(runCtx)
	go runner.Run(runCtx)

	// ждём, что проверка реально началась — иначе тест ничего бы не проверял.
	select {
	case <-checker.started:
	case <-time.After(10 * time.Second):
		runCancel()
		t.Fatal("проверка не началась — тест не дошёл до проверяемого места")
	}
	runCancel()
	runner.Close()

	states, err := svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("States = %+v, want none: обрыв по остановке записан как отказ", states)
	}
	assertNoOpenIncident(t, ctx, svc, created.ID)
}
