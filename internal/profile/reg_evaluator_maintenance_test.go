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

// mockMaint — profile.MaintenanceChecker для тестов: func-обёртка вместо
// полноценного uptime.Service (интерфейс здесь в один метод — реальный сервис
// с окнами обслуживания и своей БД тестам этого пакета не нужен). Калька
// host.mockMaint / trace.mockMaint (Task 3/5).
type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

// TestRegressionEvaluatorMaintenanceSuppressesNotify — B3 Task 6: открытие
// регрессии в окне обслуживания (Maint→true) пишет инцидент с
// InMaintenance=true, но НЕ уведомляет; закрытие того же инцидента (ещё
// внутри окна) тоже не уведомляет. Зеркало
// trace.TestEvaluatorMaintenanceSuppressesRegressionNotify (Task 5).
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
	// Канал ОБЯЗАТЕЛЕН: без него Notify не пишет outbox независимо от гейта
	// (Notifier.Notify: «проект без включённых каналов — задач не будет»), и
	// проверка outbox-очереди ниже доказывала бы только отсутствие канала, а
	// не работу гейта maintenance.
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

	// Свежее окно: slow — 80 из 100 (80%). Прошлые дни: slow — 30 из 300 (10%) →
	// база (медиана) ~0.1 → рост ≥ порога, доля ≥ пола, samples=100 → Open.
	seedProfSample(t, ch, pid, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 20, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 30, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 30, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 48*time.Hour)

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

	// Доля упала: очищаем и сеем низкую свежую долю → инцидент закрыт. Окно
	// обслуживания всё ещё активно (mockMaint не менялся) — закрытие тоже не
	// должно уведомлять.
	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 5, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 95, 5*time.Minute)
	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); open {
		t.Fatalf("regression must be resolved after recovery")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 0 {
		t.Errorf("outbox jobs after resolve tick = %d, want still 0 (close-notify suppressed too)", len(jobs))
	}
}

// TestRegressionEvaluatorMaintenanceFalseStillNotifies — Maint сконфигурирован
// (не nil), но вне окна (InMaintenance→false): поведение обычное, уведомление
// уходит. Отличает «MaintenanceChecker сконфигурирован и говорит false» от
// «MaintenanceChecker==nil» (последнее уже покрыто TestRegressionEvaluatorOpenCloseAlertOnce).
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

	seedProfSample(t, ch, pid, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 20, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 30, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 30, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 48*time.Hour)

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

// TestRegressionEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds —
// дискриминирует close-гейт «по сохранённому флагу» (!open.InMaintenance) от
// ошибочного «по текущему окну» (!e.inMaintenance(now)): открываем регрессию В
// окне, затем окно ЗАКАНЧИВАЕТСЯ (mock→false) — close всё равно должен быть
// подавлен, т.к. читается сохранённый флаг инцидента. Зеркало
// trace.TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds.
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

	seedProfSample(t, ch, pid, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 20, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 30, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 30, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 48*time.Hour)

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

	// Окно обслуживания закончилось — close-гейт должен смотреть на
	// сохранённый флаг, а не на текущее состояние окна.
	inWindow = false

	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 5, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 95, 5*time.Minute)
	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); open {
		t.Fatalf("regression must be resolved after recovery")
	}
	if jobs, _ := ob.Claim(ctx, 10); len(jobs) != 0 {
		t.Errorf("outbox jobs after resolve tick = %d, want still 0 (close by saved flag, not by current window)", len(jobs))
	}
}
