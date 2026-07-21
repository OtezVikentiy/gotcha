package profile_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRegressionEvaluatorTickCancelledCtx: на отменённом ctx первый же запрос
// «SELECT id FROM projects» падает — Tick логирует ошибку и возвращается, не
// доходя до evalProject. Покрывает ветку list-projects error.
func TestRegressionEvaluatorTickCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	eval := &profile.RegressionEvaluator{
		Pool: pool, Query: profile.NewQuery(ch),
		Regressions: profile.NewRegressionService(pool),
		Config:      profile.DefaultProfileRegressionConfig(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Не должно паниковать и не должно виснуть — просто тихо вернуться.
	eval.Tick(ctx)
}

// TestRegressionEvaluatorNilNotifier: тот же сценарий пробоя, что и в
// OpenCloseAlertOnce, но с Notifier==nil. Инцидент должен открыться, а notify()
// обязана рано выйти (Notifier==nil), не паникуя и не трогая Outbox —
// покрывает ветку `if e.Notifier == nil { return }`.
func TestRegressionEvaluatorNilNotifier(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	cfg := profile.DefaultProfileRegressionConfig()
	eval := &profile.RegressionEvaluator{
		Pool: pool, Query: profile.NewQuery(ch), Regressions: profile.NewRegressionService(pool),
		Notifier: nil, // notify() должна рано выйти
		Interval: time.Hour, Config: cfg,
	}

	// Свежее окно: slow — 80%. База прошлых дней: 10% → рост ≥ порога → Open.
	seedProfSample(t, ch, pid, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 20, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 10, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 90, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 10, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 90, 48*time.Hour)

	eval.Tick(ctx)

	if _, open, err := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); err != nil || !open {
		t.Fatalf("regression must be open even without notifier (err=%v)", err)
	}
}

// TestRegressionServiceBumpErrors: Bump по несуществующему инциденту →
// ErrRegressionNotFound (RowsAffected==0); отменённый ctx → ошибка Exec.
func TestRegressionServiceBumpErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := profile.NewRegressionService(pool)

	if err := svc.Bump(context.Background(), 999999, 0.5); !errors.Is(err, profile.ErrRegressionNotFound) {
		t.Fatalf("Bump(missing) = %v, want ErrRegressionNotFound", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Bump(ctx, 1, 0.5); err == nil {
		t.Fatal("Bump on cancelled ctx: got nil, want DB error")
	} else if errors.Is(err, profile.ErrRegressionNotFound) {
		t.Fatalf("Bump on cancelled ctx = %v, want DB error not ErrRegressionNotFound", err)
	}
}
