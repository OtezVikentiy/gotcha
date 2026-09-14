package profile_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

func TestRegressionEvaluatorMaintenanceSuppressesNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	pid := seedProject(t, pool)
	// без канала Notify не пишет outbox независимо от гейта — проверка ниже была бы пустой.
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	cfg := profile.DefaultProfileRegressionConfig()
	regressions := profile.NewRegressionService(pool)
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Notifier: &profile.RegressionNotifier{
			Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
			Regressions: regressions, Pool: pool,
		},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour, Config: cfg,
		Maint: mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil }),
	}

	// recentSamples("slow") сам по себе (150) обязан пройти MinSamples (100).
	seedProfSample(t, ch, pid, "slow", 150, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 50, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 60, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 60, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 48*time.Hour)

	eval.Tick(ctx)
	rec, open, err := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow")
	if err != nil || !open {
		t.Fatalf("regression must be open after breach: rec=%+v open=%v err=%v", rec, open, err)
	}
	if !rec.InMaintenance {
		t.Error("InMaintenance = false, want true (открыто в окне)")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 0 {
		t.Errorf("outbox jobs after open tick = %d, want 0 (suppressed by maintenance)", len(jobs))
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 100, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 2000, 5*time.Minute)
	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); open {
		t.Fatalf("regression must be resolved after recovery")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 0 {
		t.Errorf("outbox jobs after resolve tick = %d, want still 0 (close-notify suppressed too)", len(jobs))
	}
}

func TestRegressionEvaluatorMaintenanceFalseStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	pid := seedProject(t, pool)
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	cfg := profile.DefaultProfileRegressionConfig()
	regressions := profile.NewRegressionService(pool)
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Notifier: &profile.RegressionNotifier{
			Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
			Regressions: regressions, Pool: pool,
		},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour, Config: cfg,
		Maint: mockMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil }),
	}

	// recentSamples("slow") сам по себе (150) обязан пройти MinSamples (100).
	seedProfSample(t, ch, pid, "slow", 150, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 50, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 60, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 60, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 48*time.Hour)

	eval.Tick(ctx)
	rec, open, err := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow")
	if err != nil || !open {
		t.Fatalf("regression must be open after breach: rec=%+v open=%v err=%v", rec, open, err)
	}
	if rec.InMaintenance {
		t.Error("InMaintenance = true, want false (outside window)")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 1 {
		t.Errorf("outbox jobs after open tick = %d, want 1 (not suppressed outside maintenance)", len(jobs))
	}
}

func TestRegressionEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	pid := seedProject(t, pool)
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	cfg := profile.DefaultProfileRegressionConfig()
	inWindow := true
	regressions := profile.NewRegressionService(pool)
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Notifier: &profile.RegressionNotifier{
			Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
			Regressions: regressions, Pool: pool,
		},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour, Config: cfg,
		Maint: mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil }),
	}

	// recentSamples("slow") сам по себе (150) обязан пройти MinSamples (100).
	seedProfSample(t, ch, pid, "slow", 150, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 50, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 60, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 60, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 540, 48*time.Hour)

	eval.Tick(ctx)
	rec, open, err := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow")
	if err != nil || !open {
		t.Fatalf("regression must be open after breach: rec=%+v open=%v err=%v", rec, open, err)
	}
	if !rec.InMaintenance {
		t.Fatal("InMaintenance = false, want true (открыто в окне)")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 0 {
		t.Fatalf("outbox jobs after open tick = %d, want 0 (suppressed by maintenance)", len(jobs))
	}

	inWindow = false

	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 100, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 2000, 5*time.Minute)
	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); open {
		t.Fatalf("regression must be resolved after recovery")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 0 {
		t.Errorf("outbox jobs after resolve tick = %d, want still 0 (close by saved flag, not by current window)", len(jobs))
	}
}
