package uptime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// fakeNotifier collects every Event handed to it. Notify optionally returns
// a fixed err (still recording the event first) to exercise the "notify
// failed" path.
type fakeNotifier struct {
	mu     sync.Mutex
	events []uptime.Event
	err    error
}

func (f *fakeNotifier) Notify(_ context.Context, ev uptime.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.err
}

func (f *fakeNotifier) Events() []uptime.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uptime.Event, len(f.events))
	copy(out, f.events)
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

// createMonitorWith creates an http monitor with explicit regions and
// consensus policy — shared by detector tests that need multi-region setups
// (createMonitor in state_test.go always creates a single "local" region
// with the default majority consensus).
func createMonitorWith(t *testing.T, svc *uptime.Service, projectID int64, regions []string, consensus uptime.Consensus, failThreshold, recoveryThreshold int) uptime.Monitor {
	t.Helper()
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

// applyAndDetect runs one check result through ApplyResult (as the Runner
// would) and feeds the resulting State into Detector.OnResult.
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
	mon := createMonitor(t, svc, pid, 3, 2) // fail_threshold=3, single "local" region

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	now := time.Now().UTC()

	// Two fails: below fail_threshold, no incident yet.
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.Events()) != 0 {
		t.Fatalf("notified before fail_threshold reached: %+v", notifier.Events())
	}

	// Third fail reaches fail_threshold=3: incident opens, exactly one
	// "down" notification.
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

	// Two more fails: still the same incident, no new notification.
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
	mon := createMonitor(t, svc, pid, 1, 2) // fail_threshold=1 to open quickly, recovery_threshold=2

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "down!", now, nil)
	assertOpenIncident(t, ctx, svc, mon.ID)

	// Backdate started_at so the resolve below yields a non-zero duration
	// deterministically, instead of relying on real sleep.
	if _, err := pool.Exec(ctx,
		"UPDATE incidents SET started_at = started_at - interval '30 seconds' WHERE monitor_id = $1 AND resolved_at IS NULL",
		mon.ID); err != nil {
		t.Fatalf("backdate incident: %v", err)
	}

	// One ok: below recovery_threshold=2, incident stays open.
	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)

	// Second ok reaches recovery_threshold: incident resolves.
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
	mon := createMonitorWith(t, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusMajority, 1, 1)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	now := time.Now().UTC()

	// Baseline: all three regions decided and up.
	applyAndDetect(t, ctx, svc, d, mon, "r1", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r3", true, "", now, nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	// One of three down: not a majority, no incident.
	applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	// Two of three down: majority, incident opens.
	applyAndDetect(t, ctx, svc, d, mon, "r2", false, "boom", now.Add(2*time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

func TestConsensusAny(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitorWith(t, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusAny, 1, 1)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "r1", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r3", true, "", now, nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	// A single down region is enough under "any".
	applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now.Add(time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

func TestConsensusAll(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitorWith(t, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusAll, 1, 1)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "r1", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", true, "", now, nil)
	applyAndDetect(t, ctx, svc, d, mon, "r3", true, "", now, nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	// Two of three down: not all, no incident.
	applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", now.Add(time.Second), nil)
	applyAndDetect(t, ctx, svc, d, mon, "r2", false, "boom", now.Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)

	// All three down: incident opens.
	applyAndDetect(t, ctx, svc, d, mon, "r3", false, "boom", now.Add(3*time.Second), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
}

// TestConsensusIgnoresUndecidedRegions locks in the observed behaviour for
// a monitor whose other regions have never been checked (status stays
// "unknown" until they reach fail/recovery threshold): only decided
// regions enter the vote, so a single down region can already tip both
// "any" (>=1 down) and "majority" (down > decided/2, and decided=1 here).
func TestConsensusIgnoresUndecidedRegions(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pid := newProject(t, pool)

	t.Run("any", func(t *testing.T) {
		mon := createMonitorWith(t, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusAny, 1, 1)
		notifier := &fakeNotifier{}
		d := &uptime.Detector{Svc: svc, Notifier: notifier}
		// Only r1 is ever checked; r2/r3 remain "unknown".
		applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", time.Now().UTC(), nil)
		assertOpenIncident(t, ctx, svc, mon.ID)
	})

	t.Run("majority", func(t *testing.T) {
		mon := createMonitorWith(t, svc, pid, []string{"r1", "r2", "r3"}, uptime.ConsensusMajority, 1, 1)
		notifier := &fakeNotifier{}
		d := &uptime.Detector{Svc: svc, Notifier: notifier}
		applyAndDetect(t, ctx, svc, d, mon, "r1", false, "boom", time.Now().UTC(), nil)
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
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
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
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	now := time.Now().UTC()

	// Must not panic, and the incident must still be recorded even though
	// the notification delivery failed.
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false since Notify returned an error")
	}
	if len(notifier.Events()) != 1 {
		t.Fatalf("Notify attempts = %d, want 1", len(notifier.Events()))
	}
}

func TestOnResultNilNotifierOnlyTracksIncidents(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)
	d := &uptime.Detector{Svc: svc} // Notifier is nil

	st, err := svc.ApplyResult(ctx, mon.ID, "local", false, "boom", time.Now().UTC())
	if err != nil {
		t.Fatalf("ApplyResult: %v", err)
	}
	d.OnResult(ctx, mon, "local", uptime.Result{OK: false, Error: "boom"}, st) // must not panic
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
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
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

	// Simulate a previously-sent "N days left" alert, to prove it survives
	// an unchanged expiry.
	if _, err := pool.Exec(ctx, "UPDATE monitors SET ssl_alerted_days = '{14,7}' WHERE id = $1", mon.ID); err != nil {
		t.Fatalf("seed ssl_alerted_days: %v", err)
	}

	// Same expiry again: no change to ssl_expires_at or ssl_alerted_days.
	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), &expires1)

	var alerted []int
	if err := pool.QueryRow(ctx, "SELECT ssl_alerted_days FROM monitors WHERE id = $1", mon.ID).Scan(&alerted); err != nil {
		t.Fatalf("select ssl_alerted_days: %v", err)
	}
	if len(alerted) != 2 {
		t.Fatalf("ssl_alerted_days changed on unchanged expiry: %v", alerted)
	}

	// A later expiry (new certificate) clears ssl_alerted_days.
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
