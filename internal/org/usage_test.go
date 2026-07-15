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
