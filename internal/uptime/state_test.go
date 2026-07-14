package uptime_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// createMonitor creates an http monitor with custom fail/recovery
// thresholds — shared by state/incident/statuspage tests.
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

	// Partial recovery series (1 of 2) resets the fail streak but must not
	// flip status to up yet.
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

	// Partial fail series (1 of 3) resets the ok streak but must not flip
	// status back down.
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

func TestApplyResultPerRegionIndependent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 2, 2)
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
