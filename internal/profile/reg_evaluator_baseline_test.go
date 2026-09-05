package profile_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestRegressionEvaluatorThinBaselineDoesNotOpen — гейт базы через реальный
// конвейер (evalService → TopFunctionShares/BaselineFunctionShares → Decide):
// функция, чья база набрана из горстки наблюдений, не открывает регрессию,
// сколь бы велика ни была просадка; та же картина с полновесной базой —
// открывает.
//
// Объём базы считается по функции, а не по окну: свежее окно вложено в
// базовое, и оконный объём базы (здесь 300 и 3100) всегда проходит гейт вместе
// со свежим — такой гейт не срабатывал бы никогда.
func TestRegressionEvaluatorThinBaselineDoesNotOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)

	cfg := profile.DefaultProfileRegressionConfig() // Threshold 0.5, MinSamples 100, ShareFloor 0.05
	regressions := profile.NewRegressionService(pool)
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Interval: time.Hour, Config: cfg,
	}

	// Тонкая база: свежее окно slow 10 из 100 (10%, окно ≥ MinSamples), в
	// прошлые дни slow — 1 из 100 (1%). Медиана дневных долей ~1% → рост
	// в 10 раз над порогом, доля выше пола — по прежним правилам Open. Но
	// объём наблюдений slow за базу — 10+1+1 = 12 < MinSamples.
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

	// Та же просадка на полновесной базе: прошлые дни slow — 50 из 1500
	// (те же ~3%), объём slow = 10+50+50 = 110 ≥ MinSamples → Open.
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

// TestRegressionEvaluatorOpenForFunctionsErrorSkipsService — отказ PostgreSQL
// на батчевом поиске открытых инцидентов пропускает сервис, но не роняет тик:
// тик завершается и публикует живость. Моделируется закрытым пулом.
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
	// Данные есть — до OpenForFunctions тик доходит.
	seedProfSample(t, ch, 1, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, 1, "other", 20, 5*time.Minute)

	eval.Tick(ctx)
	if eval.LastTickUnix() == 0 {
		t.Fatal("tick did not finish after OpenForFunctions failure — a dead PostgreSQL must skip the service, not the tick")
	}
}
