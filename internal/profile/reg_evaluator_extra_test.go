package profile_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRegressionEvaluatorTickCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	eval := &profile.RegressionEvaluator{
		Query:       profile.NewQuery(ch),
		Regressions: profile.NewRegressionService(pool),
		Config:      profile.DefaultProfileRegressionConfig(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eval.Tick(ctx)
}

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
		Query: profile.NewQuery(ch), Regressions: profile.NewRegressionService(pool),
		Notifier: nil,
		Interval: time.Hour, Config: cfg,
	}

	// recentSamples("slow") сам по себе (150) обязан пройти MinSamples (100), не
	// только в сумме с "other"; база (60+60=120) — тоже отдельно от recent.
	seedProfSample(t, ch, pid, "slow", 150, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 50, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 60, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 60, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 48*time.Hour)

	eval.Tick(ctx)

	if _, open, err := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); err != nil || !open {
		t.Fatalf("regression must be open even without notifier (err=%v)", err)
	}
}

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
