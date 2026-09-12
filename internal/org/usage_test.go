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

	if n, err := svc.LogUsage(ctx, o.ID, now); err != nil || n != 0 {
		t.Fatalf("initial LogUsage = (%d,%v), want (0,nil)", n, err)
	}
	if granted, err := svc.CheckAndCountLogs(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("1st: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if granted, err := svc.CheckAndCountLogs(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("2nd: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if n, err := svc.LogUsage(ctx, o.ID, now); err != nil || n != 2 {
		t.Fatalf("LogUsage after 2 accepted = (%d,%v), want (2,nil)", n, err)
	}
	if granted, err := svc.CheckAndCountLogs(ctx, o.ID, now, 2, 1); err != nil || granted != 0 {
		t.Fatalf("3rd (over quota): granted=%v err=%v, want (0,nil)", granted, err)
	}
	if n, err := svc.LogUsage(ctx, o.ID, now); err != nil || n != 2 {
		t.Fatalf("LogUsage after rejected = (%d,%v), want (2,nil) (rejected must not count)", n, err)
	}

	if err := svc.SetLogQuota(ctx, o.ID, 500); err != nil {
		t.Fatalf("set log quota: %v", err)
	}
	got, _ := svc.Get(ctx, o.ID)
	if got.LogQuota != 500 {
		t.Fatalf("LogQuota = %d, want 500", got.LogQuota)
	}

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

	if d, err := svc.DroppedUsage(ctx, o.ID, now); err != nil || d != (org.Dropped{}) {
		t.Fatalf("initial dropped = (%+v,%v), want ({},nil)", d, err)
	}

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
	if err := svc.IncDroppedLogs(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("inc dropped logs: %v", err)
	}
	if err := svc.IncDroppedEvents(ctx, o.ID, now, 7); err != nil {
		t.Fatalf("inc dropped events 2: %v", err)
	}

	d, err := svc.DroppedUsage(ctx, o.ID, now)
	if err != nil {
		t.Fatalf("dropped usage: %v", err)
	}
	want := org.Dropped{Events: 12, Transactions: 3, Metrics: 2, Profiles: 1, Logs: 4}
	if d != want {
		t.Fatalf("dropped = %+v, want %+v", d, want)
	}

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

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("1st: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2, 1); err != nil || granted != 1 {
		t.Fatalf("2nd: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 2 {
		t.Fatalf("usage after 2 accepted = %d, want 2", n)
	}

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 2, 1); err != nil || granted != 0 {
		t.Fatalf("3rd (over quota): granted=%v err=%v, want (0,nil)", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 2 {
		t.Fatalf("usage after rejected = %d, want 2 (rejected must not count)", n)
	}

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, 1); err != nil || granted != 1 {
		t.Fatalf("unlimited: granted=%v err=%v, want (1,nil)", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 3 {
		t.Fatalf("usage after unlimited inc = %d, want 3", n)
	}
}

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

	if n, _ := svc.Usage(ctx, o.ID, now); n != 0 {
		t.Fatalf("events_count = %d, want 0 (untouched by other classes)", n)
	}
}

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

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 10, 4); err != nil || granted != 4 {
		t.Fatalf("пачка из 4 при квоте 10: granted=%d err=%v, want 4", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 4 {
		t.Fatalf("usage = %d, want 4 — списано не за элемент", n)
	}

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 10, 100); err != nil || granted != 6 {
		t.Fatalf("пачка из 100 при остатке 6: granted=%d err=%v, want 6", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 10 {
		t.Fatalf("usage = %d, want ровно квоту 10", n)
	}

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 10, 50); err != nil || granted != 0 {
		t.Fatalf("пачка при исчерпанной квоте: granted=%d err=%v, want 0", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 10 {
		t.Fatalf("usage = %d, want 10 — отвергнутое не должно считаться", n)
	}

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, 1000); err != nil || granted != 1000 {
		t.Fatalf("безлимит: granted=%d err=%v, want 1000", granted, err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 1010 {
		t.Fatalf("usage = %d, want 1010", n)
	}

	for _, want := range []int64{0, -5} {
		if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 0, want); err != nil || granted != 0 {
			t.Fatalf("пачка %d: granted=%d err=%v, want 0", want, granted, err)
		}
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 1010 {
		t.Fatalf("usage после пустых пачек = %d, want 1010", n)
	}
}

func TestRefundEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund", "Refund", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 100, 10); err != nil || granted != 10 {
		t.Fatalf("списание: granted=%d err=%v, want 10", granted, err)
	}
	if err := svc.RefundEvents(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 6 {
		t.Fatalf("usage после возврата 4 из 10 = %d, want 6", n)
	}
}

func TestRefundClampsAtZero(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-clamp-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-clamp", "Refund Clamp", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 100, 3); err != nil || granted != 3 {
		t.Fatalf("списание: granted=%d err=%v, want 3", granted, err)
	}
	if err := svc.RefundEvents(ctx, o.ID, now, 999); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 0 {
		t.Fatalf("usage после избыточного возврата = %d, want 0 (не ниже нуля)", n)
	}
}

func TestRefundNonPositiveNoop(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-noop-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-noop", "Refund Noop", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 100, 5); err != nil || granted != 5 {
		t.Fatalf("списание: granted=%d err=%v, want 5", granted, err)
	}
	for _, n := range []int64{0, -1} {
		if err := svc.RefundEvents(ctx, o.ID, now, n); err != nil {
			t.Fatalf("refund(%d): %v", n, err)
		}
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 5 {
		t.Fatalf("usage после no-op возвратов = %d, want 5 (без изменений)", n)
	}
}

type refundCounters struct {
	events, transactions, metrics, profiles, logs int64
}

func readRefundCounters(t *testing.T, svc *org.Service, ctx context.Context, orgID int64, month time.Time) refundCounters {
	t.Helper()
	var c refundCounters
	var err error
	if c.events, err = svc.Usage(ctx, orgID, month); err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if c.transactions, err = svc.TransactionUsage(ctx, orgID, month); err != nil {
		t.Fatalf("TransactionUsage: %v", err)
	}
	if c.metrics, err = svc.MetricUsage(ctx, orgID, month); err != nil {
		t.Fatalf("MetricUsage: %v", err)
	}
	if c.profiles, err = svc.ProfileUsage(ctx, orgID, month); err != nil {
		t.Fatalf("ProfileUsage: %v", err)
	}
	if c.logs, err = svc.LogUsage(ctx, orgID, month); err != nil {
		t.Fatalf("LogUsage: %v", err)
	}
	return c
}

func chargeAllRefundCounters(t *testing.T, svc *org.Service, ctx context.Context, orgID int64, month time.Time, n int64) {
	t.Helper()
	if granted, err := svc.CheckAndCountEvents(ctx, orgID, month, 0, n); err != nil || granted != n {
		t.Fatalf("списание events: granted=%d err=%v, want %d", granted, err, n)
	}
	if granted, err := svc.CheckAndCountTransactions(ctx, orgID, month, 0, n); err != nil || granted != n {
		t.Fatalf("списание transactions: granted=%d err=%v, want %d", granted, err, n)
	}
	if granted, err := svc.CheckAndCountMetrics(ctx, orgID, month, 0, n); err != nil || granted != n {
		t.Fatalf("списание metrics: granted=%d err=%v, want %d", granted, err, n)
	}
	if granted, err := svc.CheckAndCountProfiles(ctx, orgID, month, 0, n); err != nil || granted != n {
		t.Fatalf("списание profiles: granted=%d err=%v, want %d", granted, err, n)
	}
	if granted, err := svc.CheckAndCountLogs(ctx, orgID, month, 0, n); err != nil || granted != n {
		t.Fatalf("списание logs: granted=%d err=%v, want %d", granted, err, n)
	}
}

func TestRefundTransactions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-tx-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-tx", "Refund Tx", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()
	chargeAllRefundCounters(t, svc, ctx, o.ID, now, 10)

	if err := svc.RefundTransactions(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("refund: %v", err)
	}
	got := readRefundCounters(t, svc, ctx, o.ID, now)
	want := refundCounters{events: 10, transactions: 6, metrics: 10, profiles: 10, logs: 10}
	if got != want {
		t.Fatalf("счётчики после RefundTransactions(4) = %+v, want %+v", got, want)
	}
}

func TestRefundMetrics(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-metrics-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-metrics", "Refund Metrics", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()
	chargeAllRefundCounters(t, svc, ctx, o.ID, now, 10)

	if err := svc.RefundMetrics(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("refund: %v", err)
	}
	got := readRefundCounters(t, svc, ctx, o.ID, now)
	want := refundCounters{events: 10, transactions: 10, metrics: 6, profiles: 10, logs: 10}
	if got != want {
		t.Fatalf("счётчики после RefundMetrics(4) = %+v, want %+v", got, want)
	}
}

func TestRefundProfiles(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-profiles-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-profiles", "Refund Profiles", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()
	chargeAllRefundCounters(t, svc, ctx, o.ID, now, 10)

	if err := svc.RefundProfiles(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("refund: %v", err)
	}
	got := readRefundCounters(t, svc, ctx, o.ID, now)
	want := refundCounters{events: 10, transactions: 10, metrics: 10, profiles: 6, logs: 10}
	if got != want {
		t.Fatalf("счётчики после RefundProfiles(4) = %+v, want %+v", got, want)
	}
}

func TestRefundLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-logs-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-logs", "Refund Logs", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()
	chargeAllRefundCounters(t, svc, ctx, o.ID, now, 10)

	if err := svc.RefundLogs(ctx, o.ID, now, 4); err != nil {
		t.Fatalf("refund: %v", err)
	}
	got := readRefundCounters(t, svc, ctx, o.ID, now)
	want := refundCounters{events: 10, transactions: 10, metrics: 10, profiles: 10, logs: 6}
	if got != want {
		t.Fatalf("счётчики после RefundLogs(4) = %+v, want %+v", got, want)
	}
}

func TestRefundQueryError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "refund-err-owner@example.com")
	o, err := svc.CreateOrg(ctx, "refund-err", "Refund Err", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()
	if granted, err := svc.CheckAndCountEvents(ctx, o.ID, now, 100, 5); err != nil || granted != 5 {
		t.Fatalf("списание: granted=%d err=%v, want 5", granted, err)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := svc.RefundEvents(cancelledCtx, o.ID, now, 1); err == nil {
		t.Fatal("refund с отменённым контекстом = nil error, want ошибку")
	}
	if n, _ := svc.Usage(ctx, o.ID, now); n != 5 {
		t.Fatalf("usage после ошибочного refund = %d, want 5 (без изменений)", n)
	}
}
