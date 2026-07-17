package org_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestMetricUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "mq-owner@example.com")
	o, err := svc.CreateOrg(ctx, "mq", "MQ", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if n, err := svc.MetricUsage(ctx, o.ID, time.Now()); err != nil || n != 0 {
		t.Fatalf("initial usage = (%d,%v), want (0,nil)", n, err)
	}
	if n, _ := svc.IncMetricUsage(ctx, o.ID, time.Now()); n != 1 {
		t.Fatalf("inc = %d, want 1", n)
	}
	if n, _ := svc.IncMetricUsage(ctx, o.ID, time.Now()); n != 2 {
		t.Fatalf("inc2 = %d, want 2", n)
	}
	if err := svc.SetMetricQuota(ctx, o.ID, 500); err != nil {
		t.Fatalf("set metric quota: %v", err)
	}
	got, _ := svc.Get(ctx, o.ID)
	if got.MetricQuota != 500 {
		t.Fatalf("MetricQuota = %d, want 500", got.MetricQuota)
	}
}

func TestDroppedUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "dropped-owner@example.com")
	o, err := svc.CreateOrg(ctx, "dropped", "Dropped", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	// Пустая строка org_usage — все счётчики дропов нулевые.
	if d, err := svc.DroppedUsage(ctx, o.ID, now); err != nil || d != (org.Dropped{}) {
		t.Fatalf("initial dropped = (%+v,%v), want ({},nil)", d, err)
	}

	// Каждый счётчик инкрементируется независимо и на произвольное n.
	if err := svc.IncDroppedEvents(ctx, o.ID, now, 5); err != nil {
		t.Fatalf("inc dropped events: %v", err)
	}
	if err := svc.IncDroppedTransactions(ctx, o.ID, now, 3); err != nil {
		t.Fatalf("inc dropped transactions: %v", err)
	}
	if err := svc.IncDroppedMetrics(ctx, o.ID, now, 2); err != nil {
		t.Fatalf("inc dropped metrics: %v", err)
	}
	if err := svc.IncDroppedProfiles(ctx, o.ID, now, 1); err != nil {
		t.Fatalf("inc dropped profiles: %v", err)
	}
	// Повторный инкремент событий — суммируется (+7 → 12).
	if err := svc.IncDroppedEvents(ctx, o.ID, now, 7); err != nil {
		t.Fatalf("inc dropped events 2: %v", err)
	}

	d, err := svc.DroppedUsage(ctx, o.ID, now)
	if err != nil {
		t.Fatalf("dropped usage: %v", err)
	}
	want := org.Dropped{Events: 12, Transactions: 3, Metrics: 2, Profiles: 1}
	if d != want {
		t.Fatalf("dropped = %+v, want %+v", d, want)
	}

	// Принятые счётчики (events_count и др.) счётчиком дропов не задеты.
	if n, _ := svc.Usage(ctx, o.ID, now); n != 0 {
		t.Fatalf("events_count = %d, want 0 (drops must not touch accepted usage)", n)
	}
}

func TestProfileUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "pq-owner@example.com")
	o, err := svc.CreateOrg(ctx, "pq", "PQ", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if n, err := svc.ProfileUsage(ctx, o.ID, time.Now()); err != nil || n != 0 {
		t.Fatalf("initial = (%d,%v)", n, err)
	}
	if n, _ := svc.IncProfileUsage(ctx, o.ID, time.Now()); n != 1 {
		t.Fatalf("inc = %d, want 1", n)
	}
	if err := svc.SetProfileQuota(ctx, o.ID, 42); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	got, _ := svc.Get(ctx, o.ID)
	if got.ProfileQuota != 42 {
		t.Fatalf("ProfileQuota = %d, want 42", got.ProfileQuota)
	}
}

// TestCheckAndCountEvents проверяет условный атомарный инкремент:
// при usage==quota следующая попытка отклоняется И счётчик НЕ растёт;
// безлимит (quota=0) всегда разрешает и растит счётчик.
func TestCheckAndCountEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "cc-owner@example.com")
	o, err := svc.CreateOrg(ctx, "cc", "CC", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	// Квота 2: две попытки принимаются (счётчик 1, затем 2).
	if ok, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2); err != nil || !ok {
		t.Fatalf("1st: ok=%v err=%v, want (true,nil)", ok, err)
	}
	if ok, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2); err != nil || !ok {
		t.Fatalf("2nd: ok=%v err=%v, want (true,nil)", ok, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 2 {
		t.Fatalf("usage after 2 accepted = %d, want 2", n)
	}

	// usage==quota: третья попытка отклоняется, счётчик НЕ растёт.
	if ok, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2); err != nil || ok {
		t.Fatalf("3rd (over quota): ok=%v err=%v, want (false,nil)", ok, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 2 {
		t.Fatalf("usage after rejected = %d, want 2 (rejected must not count)", n)
	}

	// Безлимит (quota=0): всегда разрешает, счётчик продолжает расти.
	if ok, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0); err != nil || !ok {
		t.Fatalf("unlimited: ok=%v err=%v, want (true,nil)", ok, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 3 {
		t.Fatalf("usage after unlimited inc = %d, want 3", n)
	}
}

// TestCheckAndCountTransactions/Metrics/Profiles: тот же условный инкремент по
// своим колонкам, независимо от events_count.
func TestCheckAndCountOtherClasses(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "cco-owner@example.com")
	o, err := svc.CreateOrg(ctx, "cco", "CCO", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	cases := []struct {
		name  string
		check func(ctx context.Context, orgID int64, month time.Time, quota int64) (bool, error)
		usage func(ctx context.Context, orgID int64, month time.Time) (int64, error)
	}{
		{"transactions", svc.CheckAndCountTransactions, svc.TransactionUsage},
		{"metrics", svc.CheckAndCountMetrics, svc.MetricUsage},
		{"profiles", svc.CheckAndCountProfiles, svc.ProfileUsage},
	}
	for _, c := range cases {
		// Квота 1: первая принята, вторая отклонена без инкремента.
		if ok, err := c.check(ctx, o.ID, now, 1); err != nil || !ok {
			t.Fatalf("%s 1st: ok=%v err=%v, want (true,nil)", c.name, ok, err)
		}
		if ok, err := c.check(ctx, o.ID, now, 1); err != nil || ok {
			t.Fatalf("%s 2nd (over quota): ok=%v err=%v, want (false,nil)", c.name, ok, err)
		}
		if n, _ := c.usage(ctx, o.ID, now); n != 1 {
			t.Fatalf("%s usage = %d, want 1 (rejected must not count)", c.name, n)
		}
	}

	// Классы независимы: events_count не задет.
	if n, _ := svc.Usage(ctx, o.ID, now); n != 0 {
		t.Fatalf("events_count = %d, want 0 (untouched by other classes)", n)
	}
}
