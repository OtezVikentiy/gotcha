package org_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestCheckAndCountConcurrentRespectsQuota — двести параллельных списаний по
// одной единице при квоте 50 не должны выдать больше пятидесяти в сумме.
//
// Тест написан НЕ для того, чтобы упасть на текущем коде: списание корректно и
// сейчас. Он существует, чтобы упасть, если из списания уйдёт блокировка
// строки. Строка месяца одна на всю организацию, и без блокировки конкуренты
// прочитали бы одно и то же значение счётчика — квоту можно было бы превысить
// ровно во столько раз, сколько приёмов идёт одновременно. Проверено: замена
// оператора на «прочитать, потом обновить» роняет этот тест.
func TestCheckAndCountConcurrentRespectsQuota(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "conc-owner@example.com")
	o, err := svc.CreateOrg(ctx, "conc", "Conc", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	const workers = 200
	const quota = int64(50)
	var total atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := svc.CheckAndCountEvents(ctx, o.ID, now, quota, 1)
			if err != nil {
				t.Errorf("CheckAndCountEvents: %v", err)
				return
			}
			total.Add(n)
		}()
	}
	wg.Wait()

	if got := total.Load(); got != quota {
		t.Errorf("списано суммарно %d при квоте %d", got, quota)
	}
	used, err := svc.Usage(ctx, o.ID, now)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if used != quota {
		t.Errorf("events_count = %d при квоте %d: счётчик разошёлся с выданным", used, quota)
	}
}

// TestCheckAndCountConcurrentUnlimitedCountsEverything — безлимит под нагрузкой
// обязан посчитать каждую единицу: потерянное обновление здесь не превышение
// квоты, а заниженный счётчик потребления, по которому выставляют счета.
func TestCheckAndCountConcurrentUnlimitedCountsEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "conc-unl-owner@example.com")
	o, err := svc.CreateOrg(ctx, "conc-unl", "Conc Unlimited", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	const workers = 100
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, 3); err != nil || n != 3 {
				t.Errorf("CheckAndCountEvents(безлимит) = (%d, %v), want (3, nil)", n, err)
			}
		}()
	}
	wg.Wait()

	used, err := svc.Usage(ctx, o.ID, now)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if used != int64(workers*3) {
		t.Errorf("events_count = %d, want %d: часть инкрементов потеряна", used, workers*3)
	}
}

// TestCheckAndCountExhaustedDoesNotWriteRow — при исчерпанной квоте строка
// потребления не должна переписываться: приём в таком состоянии продолжает
// идти потоком, и запись на каждый отклонённый запрос грела бы журнал
// предзаписи ради нулевого результата. Версия строки (xmin) — прямой признак
// того, что запись была.
func TestCheckAndCountExhaustedDoesNotWriteRow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "exhausted-owner@example.com")
	o, err := svc.CreateOrg(ctx, "exhausted", "Exhausted", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 5, 5); err != nil || granted != 5 {
		t.Fatalf("первое списание: granted=%d err=%v, want 5", granted, err)
	}
	version := func() string {
		var xmin string
		if err := pool.QueryRow(ctx,
			"SELECT xmin::text FROM org_usage WHERE org_id = $1", o.ID).Scan(&xmin); err != nil {
			t.Fatalf("чтение версии строки: %v", err)
		}
		return xmin
	}
	before := version()

	for i := 0; i < 3; i++ {
		if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 5, 10); err != nil || granted != 0 {
			t.Fatalf("списание при исчерпанной квоте: granted=%d err=%v, want 0", granted, err)
		}
	}
	if after := version(); after != before {
		t.Errorf("строка потребления переписана при исчерпанной квоте (xmin %s → %s)", before, after)
	}
	if used, _ := svc.Usage(ctx, o.ID, now); used != 5 {
		t.Errorf("events_count = %d, want 5: отвергнутое посчитано", used)
	}
}
