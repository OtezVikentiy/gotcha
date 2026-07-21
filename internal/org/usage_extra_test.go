package org_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestIncTransactionUsage покрывает счётчик транзакций: первый инкремент создаёт
// строку (=1), повторный растит её (=2), а events_count при этом не задет —
// квоты классов независимы (см. IncTransactionUsage/Usage).
func TestIncTransactionUsage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()
	ownerID := newUser(t, pool, "inctx-owner@example.com")
	o, err := svc.CreateOrg(ctx, "inctx", "IncTx", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	now := time.Now()

	if n, err := svc.IncTransactionUsage(ctx, o.ID, now); err != nil || n != 1 {
		t.Fatalf("1st inc = (%d,%v), want (1,nil)", n, err)
	}
	if n, err := svc.IncTransactionUsage(ctx, o.ID, now); err != nil || n != 2 {
		t.Fatalf("2nd inc = (%d,%v), want (2,nil)", n, err)
	}
	if n, _ := svc.TransactionUsage(ctx, o.ID, now); n != 2 {
		t.Fatalf("TransactionUsage = %d, want 2", n)
	}
	// events_count не задет транзакциями.
	if n, _ := svc.Usage(ctx, o.ID, now); n != 0 {
		t.Fatalf("events_count = %d, want 0 (transactions must not touch events)", n)
	}
}

// TestUsageErrorBranches прогоняет ветку «ошибка запроса» у семейства usage-
// функций: отменённый контекст заставляет pool вернуть ошибку до выполнения SQL,
// поэтому каждая функция уходит в свой `return 0, fmt.Errorf(...)` / non-nil err.
// Это единственные непокрытые строки этих функций (happy-path закрыт выше).
func TestUsageErrorBranches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	now := time.Now()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // все запросы ниже вернут context.Canceled

	if _, err := svc.Usage(ctx, 1, now); err == nil {
		t.Fatal("Usage: want error on cancelled ctx")
	}
	if _, err := svc.TransactionUsage(ctx, 1, now); err == nil {
		t.Fatal("TransactionUsage: want error")
	}
	if _, err := svc.MetricUsage(ctx, 1, now); err == nil {
		t.Fatal("MetricUsage: want error")
	}
	if _, err := svc.ProfileUsage(ctx, 1, now); err == nil {
		t.Fatal("ProfileUsage: want error")
	}
	if _, err := svc.DroppedUsage(ctx, 1, now); err == nil {
		t.Fatal("DroppedUsage: want error")
	}
	if _, err := svc.IncUsage(ctx, 1, now); err == nil {
		t.Fatal("IncUsage: want error")
	}
	if _, err := svc.IncTransactionUsage(ctx, 1, now); err == nil {
		t.Fatal("IncTransactionUsage: want error")
	}
	if _, err := svc.IncMetricUsage(ctx, 1, now); err == nil {
		t.Fatal("IncMetricUsage: want error")
	}
	if _, err := svc.IncProfileUsage(ctx, 1, now); err == nil {
		t.Fatal("IncProfileUsage: want error")
	}
	if err := svc.IncDroppedEvents(ctx, 1, now, 1); err == nil {
		t.Fatal("IncDroppedEvents: want error")
	}
	if err := svc.IncDroppedTransactions(ctx, 1, now, 1); err == nil {
		t.Fatal("IncDroppedTransactions: want error")
	}
	if err := svc.IncDroppedMetrics(ctx, 1, now, 1); err == nil {
		t.Fatal("IncDroppedMetrics: want error")
	}
	if err := svc.IncDroppedProfiles(ctx, 1, now, 1); err == nil {
		t.Fatal("IncDroppedProfiles: want error")
	}
	if _, err := svc.CheckAndCountEvents(ctx, 1, now, 10); err == nil {
		t.Fatal("CheckAndCountEvents: want error")
	}
	if _, err := svc.CheckAndCountTransactions(ctx, 1, now, 10); err == nil {
		t.Fatal("CheckAndCountTransactions: want error")
	}
	if _, err := svc.CheckAndCountMetrics(ctx, 1, now, 10); err == nil {
		t.Fatal("CheckAndCountMetrics: want error")
	}
	if _, err := svc.CheckAndCountProfiles(ctx, 1, now, 10); err == nil {
		t.Fatal("CheckAndCountProfiles: want error")
	}
}

// TestSetQuotaBranches закрывает две не-happy ветки сеттеров квот: отрицательная
// квота (ErrInvalidQuota, ранний выход до SQL) и несуществующая организация
// (RowsAffected()==0 → ErrNotFound). Happy-path закрыт в usage_test.go.
func TestSetQuotaBranches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)
	ctx := context.Background()

	setters := map[string]func(context.Context, int64, int64) error{
		"metric":      svc.SetMetricQuota,
		"profile":     svc.SetProfileQuota,
		"transaction": svc.SetTransactionQuota,
		"event":       svc.SetQuota,
	}
	for name, set := range setters {
		if err := set(ctx, 1, -1); err != org.ErrInvalidQuota {
			t.Fatalf("%s quota=-1: err=%v, want ErrInvalidQuota", name, err)
		}
		// orgID=0 не существует → UPDATE затрагивает 0 строк → ErrNotFound.
		if err := set(ctx, 0, 100); err != org.ErrNotFound {
			t.Fatalf("%s missing org: err=%v, want ErrNotFound", name, err)
		}
	}
}

// TestWriteErrorBranches прогоняет ветку ошибки запроса у write-функций org
// (project/member/key) через отменённый контекст — это их непокрытые
// `return fmt.Errorf(...)` строки.
func TestWriteErrorBranches(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := org.NewService(pool, 1_000_000)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := svc.AttachTeam(ctx, 1, 1); err == nil {
		t.Fatal("AttachTeam: want error")
	}
	if err := svc.DetachTeam(ctx, 1, 1); err == nil {
		t.Fatal("DetachTeam: want error")
	}
	if err := svc.UpdateRegressionConfig(ctx, 1, "{}"); err == nil {
		t.Fatal("UpdateRegressionConfig: want error")
	}
	if err := svc.RevokeKey(ctx, 1); err == nil {
		t.Fatal("RevokeKey: want error")
	}
	if err := svc.EnsureMember(ctx, 1, 1, org.RoleMember); err == nil {
		t.Fatal("EnsureMember: want error")
	}
	if _, err := svc.CanAccessProject(ctx, 1, 1); err == nil {
		t.Fatal("CanAccessProject: want error")
	}
}
