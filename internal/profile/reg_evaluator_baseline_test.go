package profile_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestRegressionEvaluatorThinBaselineDoesNotOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)

	cfg := profile.DefaultProfileRegressionConfig()
	regressions := profile.NewRegressionService(pool)
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Interval: time.Hour, Config: cfg,
	}

	seedProfSample(t, ch, pid, "slow", 10, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 90, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 1, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 99, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 1, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 99, 48*time.Hour)

	eval.Tick(ctx)
	if _, open, err := regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); err != nil || open {
		t.Fatalf("regression opened on a thin baseline (12 samples of slow over the window): open=%v err=%v", open, err)
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 10, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 90, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 50, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 1450, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 50, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 1450, 48*time.Hour)

	eval.Tick(ctx)
	if _, open, err := regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); err != nil || !open {
		t.Fatalf("regression must open on a solid baseline (110 samples of slow): open=%v err=%v", open, err)
	}
}

func TestRegressionEvaluatorOpenForFunctionsErrorSkipsService(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	dead := testenv.MigratedPG(t)
	dead.Close()

	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: profile.NewRegressionService(dead),
		Interval: time.Hour, Config: profile.DefaultProfileRegressionConfig(),
	}
	seedProfSample(t, ch, 1, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, 1, "other", 20, 5*time.Minute)

	eval.Tick(ctx)
	if eval.LastTickUnix() == 0 {
		t.Fatal("tick did not finish after OpenForFunctions failure — a dead PostgreSQL must skip the service, not the tick")
	}
}
