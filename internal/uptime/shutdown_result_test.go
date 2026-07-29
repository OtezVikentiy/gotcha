package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// blockingChecker держит проверку до тех пор, пока не отменят контекст, —
// ровно то, что происходит с медленным HTTP-запросом в момент SIGTERM.
type blockingChecker struct{ started chan struct{} }

func (b *blockingChecker) Check(ctx context.Context, m uptime.Monitor) uptime.Result {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return uptime.Result{OK: false, Error: ctx.Err().Error()}
}

// TestShutdownDoesNotRecordFalseOutage — проверка, оборванная остановкой
// процесса, не должна записываться как отказ сервиса.
//
// Отмена ctx приходит от SIGTERM (деплой, рестарт контейнера), и проверка в
// этот момент возвращает неуспех с «context canceled». Раньше эта строка
// доезжала и в check_results, и в детектор: каждый деплой занижал аптайм, а у
// монитора с fail_threshold=1 ещё и открывал инцидент с рассылкой «сервис
// недоступен». В данных это неотличимо от настоящего падения.
func TestShutdownDoesNotRecordFalseOutage(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 1
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"local"})

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

	// Дожидаемся, что проверка реально началась, и только потом «выключаем
	// процесс» — иначе тест ничего не проверял бы.
	select {
	case <-checker.started:
	case <-time.After(10 * time.Second):
		runCancel()
		t.Fatal("проверка не началась — тест не дошёл до проверяемого места")
	}
	runCancel()
	runner.Close()

	// Ни состояния, ни инцидента: обрыв по нашей же остановке не событие о
	// сервисе.
	states, err := svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("States = %+v, want none: обрыв по остановке записан как отказ", states)
	}
	assertNoOpenIncident(t, ctx, svc, created.ID)
}
