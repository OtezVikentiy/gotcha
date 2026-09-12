package uptime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

type fakeNotifier struct {
	mu         sync.Mutex
	events     []uptime.Event
	recoveries []recoveryCall
	err        error
	svc        *uptime.Service
}

func (f *fakeNotifier) Notify(_ context.Context, ev uptime.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.err
}

func (f *fakeNotifier) NotifyOpenStep0(_ context.Context, ev uptime.Event) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	if f.err != nil {
		return nil, f.err
	}
	return []int64{1}, nil
}

func (f *fakeNotifier) NotifyRecovery(ctx context.Context, incidentID int64, channelIDs []int64) error {
	ev := uptime.Event{Kind: "up"}
	if f.svc != nil {
		if inc, ok, err := f.svc.IncidentByID(ctx, incidentID); err == nil && ok {
			ev.Incident = inc
			if inc.ResolvedAt != nil {
				ev.DurationSeconds = int64(inc.ResolvedAt.Sub(inc.StartedAt).Seconds())
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recoveries = append(f.recoveries, recoveryCall{incidentID: incidentID, channelIDs: channelIDs})
	f.events = append(f.events, ev)
	return f.err
}

type recoveryCall struct {
	incidentID int64
	channelIDs []int64
}

func (f *fakeNotifier) Events() []uptime.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uptime.Event, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeNotifier) Recoveries() []recoveryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recoveryCall, len(f.recoveries))
	copy(out, f.recoveries)
	return out
}

func (f *fakeNotifier) kindEvents(kind string) []uptime.Event {
	var out []uptime.Event
	for _, ev := range f.Events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

type fakeDepChecker struct {
	mu            sync.Mutex
	hasParent     bool
	parentDown    bool
	hasParentErr  error
	parentDownErr error
}

func (f *fakeDepChecker) HasParent(_ context.Context, _ string, _ int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasParent, f.hasParentErr
}

func (f *fakeDepChecker) ParentDown(_ context.Context, _ string, _ int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.parentDown, f.parentDownErr
}

func (f *fakeDepChecker) DownRoot(_ context.Context, _ string, _ int64) (string, int64, bool, error) {
	return "", 0, false, nil
}

func (f *fakeDepChecker) setParentDown(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parentDown = v
}

func (f *fakeDepChecker) setParentDownErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parentDownErr = err
}

func backdateIncidentStart(t *testing.T, ctx context.Context, pool *pgxpool.Pool, monitorID int64) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 seconds' WHERE monitor_id = $1 AND resolved_at IS NULL",
		monitorID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}
}

func createMonitorWith(t *testing.T, pool *pgxpool.Pool, svc *uptime.Service, projectID int64, regions []string, consensus uptime.Consensus, failThreshold, recoveryThreshold int) uptime.Monitor {
	t.Helper()
	allowRegions(t, pool, svc, context.Background(), projectID, regions)
	m := baseHTTPMonitor(projectID)
	m.FailThreshold = failThreshold
	m.RecoveryThreshold = recoveryThreshold
	m.Consensus = consensus
	m.Config = httpConfig(t, uptime.HTTPConfig{Method: "GET", URL: "https://example.com/health"})
	created, err := svc.Create(context.Background(), m, regions, nil)
	if err != nil {
		t.Fatalf("create monitor: %v", err)
	}
	return created
}

func applyAndDetect(t *testing.T, ctx context.Context, svc *uptime.Service, d *uptime.Detector, mon uptime.Monitor, region string, ok bool, errText string, at time.Time, sslExpires *time.Time) {
	t.Helper()
	st, err := svc.ApplyResult(ctx, mon.ID, region, ok, errText, at)
	if err != nil {
		t.Fatalf("ApplyResult(%s): %v", region, err)
	}
	d.OnResult(ctx, mon, region, uptime.Result{OK: ok, Error: errText, SSLExpiresAt: sslExpires}, st)
}

func assertOpenIncident(t *testing.T, ctx context.Context, svc *uptime.Service, monitorID int64) uptime.Incident {
	t.Helper()
	inc, found, err := svc.OpenIncidentFor(ctx, monitorID)
	if err != nil {
		t.Fatalf("OpenIncidentFor: %v", err)
	}
	if !found {
		t.Fatalf("want an open incident, found none")
	}
	return inc
}

func assertNoOpenIncident(t *testing.T, ctx context.Context, svc *uptime.Service, monitorID int64) {
	t.Helper()
	if _, found, err := svc.OpenIncidentFor(ctx, monitorID); err != nil {
		t.Fatalf("OpenIncidentFor: %v", err)
	} else if found {
		t.Fatalf("want no open incident, found one")
	}
}

func TestOnResultSingleRegionOpensIncidentOnceAndDedups(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.Events()) != 0 {
		t.Fatalf("notified before fail_threshold reached: %+v", notifier.Events())
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(2*time.Second), nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.Cause != "boom" {
		t.Fatalf("Incident.Cause = %q, want %q", inc.Cause, "boom")
	}
	if len(inc.Regions) != 1 || inc.Regions[0] != "local" {
		t.Fatalf("Incident.Regions = %+v, want [local]", inc.Regions)
	}
	downEvents := notifier.kindEvents("down")
	if len(downEvents) != 1 {
		t.Fatalf("down events = %d, want 1: %+v", len(downEvents), notifier.Events())
	}
	if downEvents[0].Incident.ID != inc.ID || downEvents[0].Cause != "boom" {
		t.Fatalf("down event = %+v, want incident %d cause boom", downEvents[0], inc.ID)
	}
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true after successful notify")
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(3*time.Second), nil)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(4*time.Second), nil)
	if len(notifier.kindEvents("down")) != 1 {
		t.Fatalf("down events after extra fails = %d, want still 1", len(notifier.kindEvents("down")))
	}
	inc2 := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc2.ID != inc.ID {
		t.Fatalf("a second incident was opened: %d != %d", inc2.ID, inc.ID)
	}
}

func TestOnResultResolvesIncidentWithPositiveDuration(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 2)

	notifier := &fakeNotifier{svc: svc}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "down!", now, nil)
	assertOpenIncident(t, ctx, svc, mon.ID)

	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 seconds' WHERE monitor_id = $1 AND resolved_at IS NULL",
		mon.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)

	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	upEvents := notifier.kindEvents("up")
	if len(upEvents) != 1 {
		t.Fatalf("up events = %d, want 1: %+v", len(upEvents), notifier.Events())
	}
	if upEvents[0].DurationSeconds <= 0 {
		t.Fatalf("DurationSeconds = %d, want > 0", upEvents[0].DurationSeconds)
	}
}

func TestConsensusMajority(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitorWith(t, pool, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusMajority, 1, 1)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "r1", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r3", true, "", now, nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	applyAndDetect(t, ctx, svc, d, mon, "r2", false, "boom", now.Add(2*time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

func TestConsensusAny(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitorWith(t, pool, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusAny, 1, 1)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "r1", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r3", true, "", now, nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now.Add(time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

func TestConsensusAll(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitorWith(t, pool, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusAll, 1, 1)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "r1", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r3", true, "", now, nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now.Add(time.Second), nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", false, "boom", now.Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	applyAndDetect(t, ctx, svc, d, mon, "r3", false, "boom", now.Add(3*time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

func TestConsensusUndecidedRegionsAreNotDown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	t.Run("any", func(t *testing.T) {
		mon := createMonitorWith(t, pool, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusAny, 1, 1)
		notifier := &fakeNotifier{}
		d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
		applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", time.Now().UTC(), nil)
		assertOpenIncident(t, ctx, svc, mon.ID)
	})

	t.Run("majority", func(t *testing.T) {
		mon := createMonitorWith(t, pool, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusMajority, 1, 1)
		notifier := &fakeNotifier{}
		d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
		now := time.Now().UTC()
		applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now, nil)
		assertNoOpenIncident(t, ctx, svc, mon.ID)
		applyAndDetect(t, ctx, svc, d, mon, "r2", false, "boom", now.Add(time.Second), nil)
		assertOpenIncident(t, ctx, svc, mon.ID)
	})
}

func TestOnResultInMaintenanceSuppressesNotify(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	start := time.Now().UTC().Add(-time.Hour)
	end := time.Now().UTC().Add(time.Hour)
	if _, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid,
		Name:      "maintenance",
		StartsAt:  &start,
		EndsAt:    &end,
		Timezone:  "UTC",
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.InMaintenance {
		t.Fatalf("Incident.InMaintenance = false, want true")
	}
	if len(notifier.Events()) != 0 {
		t.Fatalf("Notify called while in maintenance: %+v", notifier.Events())
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.Events()) != 0 {
		t.Fatalf("Notify called on close while in maintenance: %+v", notifier.Events())
	}
}

func TestNotifyErrorDoesNotBreakDetection(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{err: errors.New("smtp down")}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false since Notify returned an error")
	}
	if len(notifier.Events()) != 1 {
		t.Fatalf("Notify attempts = %d, want 1", len(notifier.Events()))
	}
}

func TestUptimeResolveGatedByNotifiedOpen(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	t.Run("notified_open=false: up not sent", func(t *testing.T) {
		mon := createMonitor(t, svc, pid, 1, 2)
		notifier := &fakeNotifier{err: errors.New("smtp down")}
		d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
		now := time.Now().UTC()

		applyAndDetect(t, ctx, svc, d, mon, "local", false, "down!", now, nil)
		inc := assertOpenIncident(t, ctx, svc, mon.ID)
		if inc.NotifiedOpen {
			t.Fatalf("NotifiedOpen = true, want false (open notify failed)")
		}

		applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), nil)
		applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), nil)
		assertNoOpenIncident(t, ctx, svc, mon.ID)

		if got := notifier.kindEvents("up"); len(got) != 0 {
			t.Fatalf("up events = %d, want 0: incident's down was never notified", len(got))
		}
	})

	t.Run("notified_open=true: up sent", func(t *testing.T) {
		mon := createMonitor(t, svc, pid, 1, 2)
		notifier := &fakeNotifier{}
		d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
		now := time.Now().UTC()

		applyAndDetect(t, ctx, svc, d, mon, "local", false, "down!", now, nil)
		inc := assertOpenIncident(t, ctx, svc, mon.ID)
		if !inc.NotifiedOpen {
			t.Fatalf("NotifiedOpen = false, want true")
		}

		applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), nil)
		applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), nil)
		assertNoOpenIncident(t, ctx, svc, mon.ID)

		if got := notifier.kindEvents("up"); len(got) != 1 {
			t.Fatalf("up events = %d, want 1", len(got))
		}
	})
}

func TestOnResultNilNotifierOnlyTracksIncidents(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)
	d := &uptime.Detector{Svc: svc}

	st, err := svc.ApplyResult(ctx, mon.ID, "local", false, "boom", time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	d.OnResult(ctx, mon, "local", uptime.Result{OK: false, Error: "boom"}, st)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

func TestOnResultTracksSSLExpiry(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 3, 2)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	expires1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now, &expires1)

	got, err := svc.Get(ctx, mon.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SSLExpiresAt == nil || !got.SSLExpiresAt.Equal(expires1) {
		t.Fatalf("SSLExpiresAt = %v, want %v", got.SSLExpiresAt, expires1)
	}

	if _, err := pool.Exec(ctx, "UPDATE monitors SET ssl_alerted_days = '{14,7}' WHERE id = $1", mon.ID); err != nil {
		t.Fatalf("seed ssl_alerted_days: %v", err)
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), &expires1)

	var alerted []int
	if err := pool.QueryRow(ctx, "SELECT ssl_alerted_days FROM monitors WHERE id = $1", mon.ID).Scan(&alerted); err != nil {
		t.Fatalf("select ssl_alerted_days: %v", err)
	}
	if len(alerted) != 2 {
		t.Fatalf("ssl_alerted_days changed on unchanged expiry: %v", alerted)
	}

	expires2 := expires1.Add(30 * 24 * time.Hour)
	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), &expires2)

	got, err = svc.Get(ctx, mon.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SSLExpiresAt == nil || !got.SSLExpiresAt.Equal(expires2) {
		t.Fatalf("SSLExpiresAt = %v, want %v", got.SSLExpiresAt, expires2)
	}

	alerted = nil
	if err := pool.QueryRow(ctx, "SELECT ssl_alerted_days FROM monitors WHERE id = $1", mon.ID).Scan(&alerted); err != nil {
		t.Fatalf("select ssl_alerted_days: %v", err)
	}
	if len(alerted) != 0 {
		t.Fatalf("ssl_alerted_days not cleared after later expiry: %v", alerted)
	}
}

func TestUptimeChildHeldThenSuppressed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: monitor has a declared parent, notify must be held")
	}
	if len(notifier.Events()) != 0 {
		t.Fatalf("notified synchronously on open despite a declared parent: %+v", notifier.Events())
	}

	dep.setParentDown(true)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)
	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = false, want true once ParentDown=true")
	}
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: incident was suppressed, never notified")
	}
	if len(notifier.kindEvents("down")) != 0 {
		t.Fatalf("down event sent despite dependency suppression: %+v", notifier.Events())
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if got := notifier.kindEvents("up"); len(got) != 0 {
		t.Fatalf("up events = %d, want 0: down was suppressed by dependency, never delivered", len(got))
	}
}

func TestDetectorReleasesSuppressedIncidentWhenParentRecovers(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: monitor has a declared parent, notify must be held")
	}

	dep.setParentDown(true)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)
	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = false, want true once ParentDown=true")
	}

	backdateIncidentStart(t, ctx, pool, mon.ID)
	dep.setParentDown(false)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(2*time.Second), nil)
	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = true, want false: parent recovered, must be released")
	}
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: incident already cleared SettleGrace on its own clock")
	}
	if got := notifier.kindEvents("down"); len(got) != 1 {
		t.Fatalf("down events = %d, want 1 (released incident must notify exactly once)", len(got))
	}

	var depReleasedAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT dep_released_at FROM incidents WHERE id = $1", inc.ID).Scan(&depReleasedAt); err != nil {
		t.Fatalf("select dep_released_at: %v", err)
	}
	if depReleasedAt == nil {
		t.Fatal("dep_released_at = NULL after release, want the moment of release stamped")
	}
}

func TestDetectorStaysUnnotifiedInsideGraceAfterParentRecovers(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	dep.setParentDown(true)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = false, want true once ParentDown=true")
	}

	dep.setParentDown(false)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(2*time.Second), nil)
	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = true, want false: parent recovered, dependency no longer suppresses it")
	}
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: incident has not cleared SettleGrace on its own clock yet")
	}
	if got := notifier.kindEvents("down"); len(got) != 0 {
		t.Fatalf("down events = %d, want 0: a parent blip shorter than SettleGrace must not page", len(got))
	}

	var depReleasedAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT dep_released_at FROM incidents WHERE id = $1", inc.ID).Scan(&depReleasedAt); err != nil {
		t.Fatalf("select dep_released_at: %v", err)
	}
	if depReleasedAt == nil {
		t.Fatal("dep_released_at = NULL after release, want the moment of release stamped even though notification is still held")
	}
}

func TestDetectorStaysSuppressedWhileParentDown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	dep.setParentDown(true)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = false, want true once ParentDown=true")
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(2*time.Second), nil)
	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = false, want true: parent is still down")
	}
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: incident stays suppressed")
	}
	if got := notifier.kindEvents("down"); len(got) != 0 {
		t.Fatalf("down events = %d, want 0: incident must stay silent while the parent is down", len(got))
	}
}

func TestUptimeChildNotifiesAfterGrace(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.Events()) != 0 {
		t.Fatalf("notified synchronously on open despite a declared parent: %+v", notifier.Events())
	}

	backdateIncidentStart(t, ctx, pool, mon.ID)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: settling grace elapsed with the parent still up")
	}
	if inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = true, want false: parent stayed up throughout")
	}
	downEvents := notifier.kindEvents("down")
	if len(downEvents) != 1 {
		t.Fatalf("down events = %d, want 1: %+v", len(downEvents), notifier.Events())
	}
	if downEvents[0].Incident.ID != inc.ID || downEvents[0].Cause != "boom" {
		t.Fatalf("down event = %+v, want incident %d cause boom", downEvents[0], inc.ID)
	}
}

func TestUptimeNoParentNotifiesImmediately(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: monitor has no parent, notify must stay synchronous")
	}
	downEvents := notifier.kindEvents("down")
	if len(downEvents) != 1 {
		t.Fatalf("down events = %d, want 1: %+v", len(downEvents), notifier.Events())
	}
}

// ошибка HasParent — узел считается без родителя и уведомляется немедленно:
// молчать про реальное падение хуже лишнего уведомления.
func TestUptimeHasParentErrorNotifiesImmediately(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, hasParentErr: errors.New("dep db down")}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: ошибка HasParent должна fail-safe уведомлять сразу")
	}
	if got := notifier.kindEvents("down"); len(got) != 1 {
		t.Fatalf("down events = %d, want 1 (fail-safe notify): %+v", len(got), notifier.Events())
	}
}

// ошибка ParentDown не приравнивается к «родитель упал»: если грейс уже
// истёк, детектор шлёт отложенный "down", а не подавляет инцидент.
func TestUptimeParentDownErrorNotifiesAfterGrace(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.Events()) != 0 {
		t.Fatalf("notified synchronously despite a declared parent: %+v", notifier.Events())
	}

	dep.setParentDownErr(errors.New("dep db down"))
	backdateIncidentStart(t, ctx, pool, mon.ID)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.SuppressedByDep {
		t.Fatalf("SuppressedByDep = true, want false: ошибка ParentDown не должна подавлять")
	}
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: грейс истёк при ошибке ParentDown → fail-safe notify")
	}
	if got := notifier.kindEvents("down"); len(got) != 1 {
		t.Fatalf("down events = %d, want 1 (fail-safe notify after grace): %+v", len(got), notifier.Events())
	}
}

// подавление окном обслуживания не снимается автоматом грейса зависимостей —
// инцидент остаётся неуведомлённым даже когда грейс истёк.
func TestUptimeMaintenanceNotResurrected(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	start := time.Now().UTC().Add(-time.Hour)
	end := time.Now().UTC().Add(time.Hour)
	if _, err := svc.CreateWindow(ctx, uptime.Window{
		ProjectID: pid,
		Name:      "maintenance",
		StartsAt:  &start,
		EndsAt:    &end,
		Timezone:  "UTC",
	}); err != nil {
		t.Fatalf("CreateWindow: %v", err)
	}

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: false}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.InMaintenance {
		t.Fatalf("InMaintenance = false, want true")
	}
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false")
	}

	backdateIncidentStart(t, ctx, pool, mon.ID)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)

	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: maintenance suppression must not be resurrected")
	}
	if len(notifier.Events()) != 0 {
		t.Fatalf("Notify called despite maintenance window: %+v", notifier.Events())
	}
}
