package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestRemovedRegionStopsHoldingMonitorDown — снятый регион не должен держать
// монитор в down навсегда.
//
// Update перезаписывал список регионов, но строку состояния снятого региона не
// трогал, а перезаписать её больше некому: задание для этого региона не
// ставится. Регион, зафиксированный в «down», делал монитор красным навсегда —
// при consensus=any хватает одного down. Ветка «всё поднялось» становилась
// недостижимой: инцидент не закрывался, напоминания шли бесконечно, и то же
// самое видел посетитель публичной статус-страницы.
func TestRemovedRegionStopsHoldingMonitorDown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 1
	m.RecoveryThreshold = 1
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created := mustCreateMonitor(t, svc, ctx, m, []string{"eu", "us"})

	// us лёг, eu поднялся.
	now := time.Now().UTC()
	if _, err := svc.ApplyResult(ctx, created.ID, "us", false, "boom", now); err != nil {
		t.Fatalf("ApplyResult us: %v", err)
	}
	if _, err := svc.ApplyResult(ctx, created.ID, "eu", true, "", now); err != nil {
		t.Fatalf("ApplyResult eu: %v", err)
	}
	states, err := svc.States(ctx, created.ID)
	if err != nil || len(states) != 2 {
		t.Fatalf("States = %+v err=%v, want two", states, err)
	}

	// Оператор убирает us из монитора.
	updated := created
	updated.Config = m.Config
	if err := svc.Update(ctx, updated, []string{"eu"}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}

	states, err = svc.States(ctx, created.ID)
	if err != nil {
		t.Fatalf("States after update: %v", err)
	}
	if len(states) != 1 || states[0].Region != "eu" {
		t.Fatalf("States after removing us = %+v, want only eu", states)
	}
	if states[0].Status != "up" {
		t.Fatalf("оставшийся регион = %q, want up", states[0].Status)
	}

	// Батч-версия видит то же самое: список мониторов и публичная
	// статус-страница ходят через неё.
	batch, err := svc.StatesBatch(ctx, []int64{created.ID})
	if err != nil {
		t.Fatalf("StatesBatch: %v", err)
	}
	if got := batch[created.ID]; len(got) != 1 || got[0].Region != "eu" {
		t.Fatalf("StatesBatch = %+v, want only eu", got)
	}

	// Задание снятого региона тоже не остаётся: иначе его один раз возьмут в
	// лизу и выполнят уже после того, как регион убрали.
	var queued int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM check_queue WHERE monitor_id = $1 AND region = 'us'", created.ID).Scan(&queued); err != nil {
		t.Fatalf("count check_queue: %v", err)
	}
	if queued != 0 {
		t.Fatalf("в очереди осталось %d заданий снятого региона", queued)
	}
}
