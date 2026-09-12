package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func createMonitor(t *testing.T, svc *uptime.Service, projectID int64, failThreshold, recoveryThreshold int) uptime.Monitor {
	t.Helper()
	ctx := context.Background()
	m := baseHTTPMonitor(projectID)
	m.FailThreshold = failThreshold
	m.RecoveryThreshold = recoveryThreshold
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(ctx, m, []string{"local"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	return created
}

func TestApplyResultTransitionTable(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)
	now := time.Now().UTC().Truncate(time.Second)

	st, err := svc.ApplyResult(ctx, mon.ID, "local", false, "timeout", now)
	if err != nil {
		t.Fatalf("ApplyResult 1st fail: %v", err)
	}
	if st.Status != "unknown" || st.ConsecutiveFails != 1 || st.ConsecutiveOKs != 0 {
		t.Fatalf("after 1 fail: %+v, want status=unknown fails=1 oks=0", st)
	}

	st, err = svc.ApplyResult(ctx, mon.ID, "local", false, "timeout", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ApplyResult 2nd fail: %v", err)
	}
	if st.Status != "unknown" || st.ConsecutiveFails != 2 {
		t.Fatalf("after 2 fails: %+v, want status=unknown fails=2", st)
	}

	st, err = svc.ApplyResult(ctx, mon.ID, "local", false, "timeout", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("ApplyResult 3rd fail: %v", err)
	}
	if st.Status != "down" || st.ConsecutiveFails != 3 {
		t.Fatalf("after 3 fails: %+v, want status=down fails=3", st)
	}

	// 1 из 2 успехов — сбрасывает серию провалов, но status ещё не up.
	st, err = svc.ApplyResult(ctx, mon.ID, "local", true, "", now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("ApplyResult 1st ok: %v", err)
	}
	if st.Status != "down" || st.ConsecutiveOKs != 1 || st.ConsecutiveFails != 0 {
		t.Fatalf("after 1 ok: %+v, want status=down oks=1 fails=0", st)
	}

	st, err = svc.ApplyResult(ctx, mon.ID, "local", true, "", now.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("ApplyResult 2nd ok: %v", err)
	}
	if st.Status != "up" || st.ConsecutiveOKs != 2 {
		t.Fatalf("after 2 oks: %+v, want status=up oks=2", st)
	}
	if st.LastError != "" {
		t.Fatalf("after ok: LastError = %q, want empty", st.LastError)
	}

	// 1 из 3 провалов — сбрасывает серию успехов, status ещё не down.
	st, err = svc.ApplyResult(ctx, mon.ID, "local", false, "boom", now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ApplyResult partial fail: %v", err)
	}
	if st.Status != "up" || st.ConsecutiveFails != 1 || st.ConsecutiveOKs != 0 || st.LastError != "boom" {
		t.Fatalf("after partial fail: %+v, want status=up fails=1 oks=0 lastError=boom", st)
	}
	if st.LastCheckedAt == nil || !st.LastCheckedAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("LastCheckedAt = %v, want %v", st.LastCheckedAt, now.Add(5*time.Minute))
	}

	states, err := svc.States(ctx, mon.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 1 || states[0].Region != "local" || states[0].Status != "up" {
		t.Fatalf("States: %+v", states)
	}
}

func TestApplyResultDoesNotRollBackLastChecked(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)
	fresh := time.Now().UTC().Truncate(time.Second)
	late := fresh.Add(-5 * time.Minute)

	if _, err := svc.ApplyResult(ctx, mon.ID, "local", true, "", fresh); err != nil {
		t.Fatalf("свежий результат: %v", err)
	}
	got, err := svc.ApplyResult(ctx, mon.ID, "local", false, "dial timeout", late)
	if err != nil {
		t.Fatalf("запоздавший результат: %v", err)
	}

	if got.LastCheckedAt == nil || got.LastCheckedAt.Before(fresh) {
		t.Fatalf("LastCheckedAt = %v, откатился назад к запоздавшему %v при свежем %v",
			got.LastCheckedAt, late, fresh)
	}
	if got.LastError == "dial timeout" {
		t.Fatal("LastError взят из запоздавшего результата: текст ошибки " +
			"разошёлся со временем, к которому относится")
	}
}

// не то же, что TestApplyResultDoesNotRollBackLastChecked: тот гейт защищает
// last_checked_at/last_error, этот — прицельно counters/status от заражения серии.
func TestApplyResultDoesNotContaminateSeriesWithStaleResult(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 2, 2) // fail_threshold=2, recovery_threshold=2
	recent := time.Now().UTC().Truncate(time.Second)
	stale := recent.Add(-5 * time.Minute)

	first, err := svc.ApplyResult(ctx, mon.ID, "local", false, "первый провал", recent)
	if err != nil {
		t.Fatalf("первый провал: %v", err)
	}
	if first.ConsecutiveFails != 1 || first.Status == "down" {
		t.Fatalf("после первого провала: %+v, хотел fails=1 и status != down", first)
	}

	// без гейта на счётчиках этот запоздавший провал дотянул бы серию до
	// fail_threshold=2 — хотя по времени он случился ДО уже учтённого первого.
	got, err := svc.ApplyResult(ctx, mon.ID, "local", false, "запоздавший провал", stale)
	if err != nil {
		t.Fatalf("запоздавший провал: %v", err)
	}

	if got.ConsecutiveFails != 1 {
		t.Fatalf("ConsecutiveFails = %d, хотел 1 (запоздавший провал не должен "+
			"дотягивать серию до fail_threshold=2)", got.ConsecutiveFails)
	}
	if got.Status == "down" {
		t.Fatalf("Status = %q, монитор ушёл в down из-за запоздавшего провала, "+
			"который по счёту не должен был идти вторым в серии", got.Status)
	}
	if got.LastError == "запоздавший провал" {
		t.Fatal("LastError взят из запоздавшего результата")
	}
}

// гейт в SQL — две независимые CASE-ветки; регресс только ветки успехов не
// поймать без зеркала. Исходный status=unknown, иначе переход в up был бы не виден.
func TestApplyResultDoesNotContaminateSeriesWithStaleOK(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 2, 2) // fail_threshold=2, recovery_threshold=2
	recent := time.Now().UTC().Truncate(time.Second)
	stale := recent.Add(-5 * time.Minute)

	first, err := svc.ApplyResult(ctx, mon.ID, "local", true, "", recent)
	if err != nil {
		t.Fatalf("первый успех: %v", err)
	}
	if first.ConsecutiveOKs != 1 || first.Status == "up" {
		t.Fatalf("после первого успеха: %+v, хотел oks=1 и status != up", first)
	}

	// без гейта на счётчиках этот запоздавший успех дотянул бы серию до
	// recovery_threshold=2 — хотя по времени он случился ДО уже учтённого первого.
	got, err := svc.ApplyResult(ctx, mon.ID, "local", true, "", stale)
	if err != nil {
		t.Fatalf("запоздавший успех: %v", err)
	}

	if got.ConsecutiveOKs != 1 {
		t.Fatalf("ConsecutiveOKs = %d, хотел 1 (запоздавший успех не должен "+
			"дотягивать серию до recovery_threshold=2)", got.ConsecutiveOKs)
	}
	if got.Status == "up" {
		t.Fatalf("Status = %q, монитор ушёл в up из-за запоздавшего успеха, "+
			"который по счёту не должен был идти вторым в серии", got.Status)
	}
}

func TestApplyResultPerRegionIndependent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	// регионы монитора те же, что в ApplyResult — иначе тест проверял бы
	// независимость регионов на сироте (JOIN её исключает), а не по-настоящему.
	m := baseHTTPMonitor(pid)
	m.FailThreshold = 2
	m.RecoveryThreshold = 2
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	allowRegions(t, pool, svc, ctx, pid, []string{"eu", "us"})
	mon, err := svc.Create(ctx, m, []string{"eu", "us"}, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	now := time.Now().UTC()

	if _, err := svc.ApplyResult(ctx, mon.ID, "eu", false, "x", now); err != nil {
		t.Fatalf("ApplyResult eu: %v", err)
	}
	if _, err := svc.ApplyResult(ctx, mon.ID, "us", true, "", now); err != nil {
		t.Fatalf("ApplyResult us: %v", err)
	}

	states, err := svc.States(ctx, mon.ID)
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("States: %+v, want 2 regions", states)
	}
	byRegion := map[string]uptime.State{}
	for _, s := range states {
		byRegion[s.Region] = s
	}
	if byRegion["eu"].ConsecutiveFails != 1 || byRegion["us"].ConsecutiveOKs != 1 {
		t.Fatalf("per-region state not independent: %+v", byRegion)
	}
}
