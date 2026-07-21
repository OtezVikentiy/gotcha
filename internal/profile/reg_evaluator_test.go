package profile_test

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedProfSample(t *testing.T, ch driver.Conn, projectID int64, leaf string, v uint64, ago time.Duration) {
	t.Helper()
	if err := ch.Exec(context.Background(), `INSERT INTO profile_samples
		(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
		VALUES (?,'cpu','api','','','go',?,?,?,'')`,
		projectID, time.Now().UTC().Add(-ago), []string{"root", leaf}, v); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestRegressionEvaluatorRun покрывает жизненный цикл тикера Run: с реальным
// (но пустым) списком проектов тикер срабатывает несколько раз за короткий
// интервал (ветка tick.C→Tick), а отмена контекста корректно завершает цикл
// (ветка ctx.Done). Отдельный прогон с Interval=0 закрывает подстановку
// значения по умолчанию (evaluatorDefaultInterval).
func TestRegressionEvaluatorRun(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	eval := &profile.RegressionEvaluator{
		Pool: pool, Query: profile.NewQuery(ch),
		Regressions: profile.NewRegressionService(pool),
		Interval:    2 * time.Millisecond,
		Config:      profile.DefaultProfileRegressionConfig(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eval.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond) // дать тикеру сработать несколько раз
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}

	// Interval<=0 → используется evaluatorDefaultInterval (5m): тикер не успеет
	// сработать, но ветка выбора интервала выполняется; отмена завершает цикл.
	eval.Interval = 0
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { eval.Run(ctx2); close(done2) }()
	cancel2()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("Run(Interval=0) не завершился после отмены")
	}
}

func TestRegressionEvaluatorOpenCloseAlertOnce(t *testing.T) {
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
		t.Fatalf("channel: %v", err)
	}

	cfg := profile.DefaultProfileRegressionConfig() // Threshold 0.5, MinSamples 100, ShareFloor 0.05, BaselineDays 7
	eval := &profile.RegressionEvaluator{
		Pool: pool, Query: profile.NewQuery(ch), Regressions: profile.NewRegressionService(pool),
		Notifier: &profile.RegressionNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example"},
		Interval: time.Hour, Config: cfg,
	}

	// Свежее окно: slow — 80 из 100 (80%). Прошлые дни: slow — 10 из 100 (10%) →
	// база (медиана) ~0.1 → рост +700% ≥ порога, доля ≥ пола, samples=100 → Open.
	seedProfSample(t, ch, pid, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 20, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 10, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 90, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 10, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 90, 48*time.Hour)

	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); !open {
		t.Fatalf("regression must be open after breach")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("open jobs = %d, want 1", len(jobs))
	}

	// Повторный тик — новых уведомлений нет (алерт один раз).
	eval.Tick(ctx)
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 0 {
		t.Fatalf("re-tick produced %d jobs, want 0", len(jobs2))
	}

	// Доля упала: очищаем и сеем низкую свежую долю → инцидент закрыт, одно
	// уведомление о закрытии.
	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 5, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 95, 5*time.Minute)
	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); open {
		t.Fatalf("regression must be resolved after recovery")
	}
	jobs3, _ := ob.Claim(ctx, 10)
	if len(jobs3) != 1 {
		t.Fatalf("close jobs = %d, want 1", len(jobs3))
	}
}
