package org_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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

// TestLogUsage — CheckAndCountLogs/SetLogQuota по образцу TestMetricUsage.
// LogUsage-геттера у сервиса нет (C1 не заводит его — Dropped/DroppedUsage
// тоже не трогаем), поэтому logs_count читается напрямую из org_usage.
func TestLogUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "lq-owner@example.com")
	o, err := svc.CreateOrg(ctx, "lq", "LQ", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	logsCount := func() int64 {
		var n int64
		err := pool.QueryRow(ctx,
			"SELECT logs_count FROM org_usage WHERE org_id = $1 AND period_month = $2",
			o.ID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0
		}
		if err != nil {
			t.Fatalf("select logs_count: %v", err)
		}
		return n
	}

	if n := logsCount(); n != 0 {
		t.Fatalf("initial logs_count = %d, want 0", n)
	}
	if granted, err := svc.CheckAndCountLogs(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("1st: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if granted, err := svc.CheckAndCountLogs(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("2nd: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if n := logsCount(); n != 2 {
		t.Fatalf("logs_count after 2 accepted = %d, want 2", n)
	}
	// Квота исчерпана: третья попытка отклоняется, счётчик не растёт.
	if granted, err := svc.CheckAndCountLogs(ctx, o.ID, now, 2, 1); err != nil || granted != 0 {
		t.Fatalf("3rd (over quota): granted=%v err=%v, want (0,nil)", granted, err)
	}
	if n := logsCount(); n != 2 {
		t.Fatalf("logs_count after rejected = %d, want 2 (rejected must not count)", n)
	}

	if err := svc.SetLogQuota(ctx, o.ID, 500); err != nil {
		t.Fatalf("set log quota: %v", err)
	}
	got, _ := svc.Get(ctx, o.ID)
	if got.LogQuota != 500 {
		t.Fatalf("LogQuota = %d, want 500", got.LogQuota)
	}

	// Независимость классов: events_count логами не задет.
	if n, _ := svc.Usage(ctx, o.ID, now); n != 0 {
		t.Fatalf("events_count = %d, want 0 (untouched by logs)", n)
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
	// IncDroppedLogs — та же схема, но logs_count/dropped_logs не входят в
	// Dropped/DroppedUsage (C1 их не трогает), поэтому проверяется отдельным
	// прямым запросом ниже.
	if err := svc.IncDroppedLogs(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("inc dropped logs: %v", err)
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

	var droppedLogs int64
	if err := pool.QueryRow(ctx,
		"SELECT dropped_logs FROM org_usage WHERE org_id = $1 AND period_month = $2",
		o.ID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)).Scan(&droppedLogs); err != nil {
		t.Fatalf("select dropped_logs: %v", err)
	}
	if droppedLogs != 4 {
		t.Fatalf("dropped_logs = %d, want 4", droppedLogs)
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
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("1st: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("2nd: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 2 {
		t.Fatalf("usage after 2 accepted = %d, want 2", n)
	}

	// usage==quota: третья попытка отклоняется, счётчик НЕ растёт.
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2, 1); err != nil || granted != 0 {
		t.Fatalf("3rd (over quota): granted=%v err=%v, want (0,nil)", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 2 {
		t.Fatalf("usage after rejected = %d, want 2 (rejected must not count)", n)
	}

	// Безлимит (quota=0): всегда разрешает, счётчик продолжает расти.
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, 1); err != nil || granted != 1 {
		t.Fatalf("unlimited: granted=%v err=%v, want (1,nil)", granted, err)
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
		check func(ctx context.Context, orgID int64, month time.Time, quota, want int64) (int64, error)
		usage func(ctx context.Context, orgID int64, month time.Time) (int64, error)
	}{
		{"transactions", svc.CheckAndCountTransactions, svc.TransactionUsage},
		{"metrics", svc.CheckAndCountMetrics, svc.MetricUsage},
		{"profiles", svc.CheckAndCountProfiles, svc.ProfileUsage},
	}
	for _, c := range cases {
		// Квота 1: первая принята, вторая отклонена без инкремента.
		if granted, err := c.check(ctx, o.ID, now, 1, 1); err != nil || granted != 1 {
			t.Fatalf("%s 1st: granted=%v err=%v, want (1,nil)", c.name, granted, err)
		}
		if granted, err := c.check(ctx, o.ID, now, 1, 1); err != nil || granted != 0 {
			t.Fatalf("%s 2nd (over quota): granted=%v err=%v, want (0,nil)", c.name, granted, err)
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

// TestCheckAndCountPartialGrant — квота списывается ЗА ЭЛЕМЕНТ, и списание
// частичное.
//
// Раньше списывалась единица за HTTP-ЗАПРОС: конверт с тысячей событий или
// экспорт с десятью тысячами OTLP-спанов стоил ровно столько же, сколько одно
// событие. Квоту можно было обойти на четыре порядка, а org_usage — то, по чему
// оператор судит о потреблении, — врал на столько же.
//
// Частичность важна не меньше: если до квоты осталось меньше, чем в пачке,
// организация должна получить остаток, а не «последняя пачка целиком мимо».
func TestCheckAndCountPartialGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "partial-owner@example.com")
	o, err := svc.CreateOrg(ctx, "partial", "Partial", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	// Квота 10, просим 4 — влезает всё.
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 10, 4); err != nil || granted != 4 {
		t.Fatalf("пачка из 4 при квоте 10: granted=%d err=%v, want 4", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 4 {
		t.Fatalf("usage = %d, want 4 — списано не за элемент", n)
	}

	// Просим 100, осталось 6 — влезает ровно остаток.
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 10, 100); err != nil || granted != 6 {
		t.Fatalf("пачка из 100 при остатке 6: granted=%d err=%v, want 6", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 10 {
		t.Fatalf("usage = %d, want ровно квоту 10", n)
	}

	// Квота выбрана — следующая пачка не даёт ничего и счётчик не растёт.
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 10, 50); err != nil || granted != 0 {
		t.Fatalf("пачка при исчерпанной квоте: granted=%d err=%v, want 0", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 10 {
		t.Fatalf("usage = %d, want 10 — отвергнутое не должно считаться", n)
	}

	// Безлимит: списывается вся пачка целиком.
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, 1000); err != nil || granted != 1000 {
		t.Fatalf("безлимит: granted=%d err=%v, want 1000", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 1010 {
		t.Fatalf("usage = %d, want 1010", n)
	}

	// Нулевая и отрицательная пачка не трогают счётчик.
	for _, want := range []int64{0, -5} {
		if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, want); err != nil || granted != 0 {
			t.Fatalf("пачка %d: granted=%d err=%v, want 0", want, granted, err)
		}
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 1010 {
		t.Fatalf("usage после пустых пачек = %d, want 1010", n)
	}
}
