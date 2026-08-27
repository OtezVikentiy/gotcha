package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// TestUptimeNotifyOpenFailedRetriesUntilDelivered — W2-C находка 1: a failed
// "down" delivery must not be a dead end. The first attempt (openIncident,
// synchronous) fails; the very next "still down" tick retries via
// settleHeldIncident and succeeds — NotifiedOpen ends up true. This also
// demonstrates the "outage shorter than SettleGrace still gets notified in
// the end" requirement: SettleGrace here is a full hour, far longer than the
// incident's actual lifetime, and delivery still succeeds because the retry
// path used for a failed attempt does not wait on SettleGrace at all — only
// the B5 held-child path does.
func TestUptimeNotifyOpenFailedRetriesUntilDelivered(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1) // fail_threshold=1, recovery_threshold=1

	notifier := &fakeNotifier{err: errors.New("smtp down")}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, SettleGrace: time.Hour, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: first attempt failed")
	}
	if !inc.NotifyOpenFailed {
		t.Fatalf("NotifyOpenFailed = false, want true: first attempt failed")
	}
	if inc.NotifyOpenAttempts != 1 {
		t.Fatalf("NotifyOpenAttempts = %d, want 1", inc.NotifyOpenAttempts)
	}
	if len(notifier.kindEvents("down")) != 1 {
		t.Fatalf("down attempts = %d, want 1", len(notifier.kindEvents("down")))
	}

	// Channel recovers; the next "still down" tick must retry immediately —
	// not wait for the (huge) SettleGrace.
	notifier.err = nil
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)

	inc = assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: retry should have delivered")
	}
	if inc.NotifyOpenFailed {
		t.Fatalf("NotifyOpenFailed = true, want false: delivery succeeded, flag must clear")
	}
	if len(notifier.kindEvents("down")) != 2 {
		t.Fatalf("down attempts = %d, want 2 (initial + retry)", len(notifier.kindEvents("down")))
	}

	// The incident resolves right after — "up" must now go out, because
	// "down" did, in the end, get delivered (NotifiedOpen=true).
	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if got := notifier.kindEvents("up"); len(got) != 1 {
		t.Fatalf("up events = %d, want 1: down was delivered via retry", len(got))
	}
}

// TestUptimeNotifyOpenFailedRetriesWithoutDep — regression for the audit's
// literal claim ("only if the dep service is up"): the retry path for a
// failed delivery must not require d.Dep != nil. d.Dep IS always non-nil in
// production (cmd/gotcha/main.go wires the same depsuppress.Suppressor into
// every uptimeDetector unconditionally in modes web/uptime/all — see W2-C
// report), but the retry logic itself must not silently depend on that
// production wiring detail: a nil Dep here must retry exactly like a
// configured one.
func TestUptimeNotifyOpenFailedRetriesWithoutDep(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{err: errors.New("smtp down")}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool} // Dep left nil, SettleGrace left zero
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	assertOpenIncident(t, ctx, svc, mon.ID)

	notifier.err = nil
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: retry must fire even with Dep == nil")
	}
}

// TestUptimeNotifyOpenFailedRetryBoundStopsRetrying — W2-C находка 1: bounded
// retries (a permanently dead channel must not be paged forever). Notify
// fails on every attempt; after maxNotifyOpenAttempts (5, see
// internal/uptime/detector.go) the retry loop must stop calling Notify.
func TestUptimeNotifyOpenFailedRetryBoundStopsRetrying(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{err: errors.New("smtp permanently down")}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil) // attempt 1 (openIncident)
	for i := 2; i <= 7; i++ {                                             // attempts 2..7 via settleHeldIncident retries
		applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Duration(i)*time.Second), nil)
	}

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = true, want false: channel never recovered")
	}
	if inc.NotifyOpenAttempts != 5 {
		t.Fatalf("NotifyOpenAttempts = %d, want 5: retries must stop at the bound", inc.NotifyOpenAttempts)
	}
	if got := len(notifier.kindEvents("down")); got != 5 {
		t.Fatalf("Notify called %d times, want exactly 5 (bound respected, no further attempts)", got)
	}
}
