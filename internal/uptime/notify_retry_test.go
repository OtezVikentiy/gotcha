package uptime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

func TestUptimeNotifyOpenFailedRetriesUntilDelivered(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

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

	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(2*time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if got := notifier.kindEvents("up"); len(got) != 1 {
		t.Fatalf("up events = %d, want 1: down was delivered via retry", len(got))
	}
}

func TestUptimeNotifyOpenFailedRetriesWithoutDep(t *testing.T) {
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
	assertOpenIncident(t, ctx, svc, mon.ID)

	notifier.err = nil
	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now.Add(time.Second), nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !inc.NotifiedOpen {
		t.Fatalf("NotifiedOpen = false, want true: retry must fire even with Dep == nil")
	}
}

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

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	for i := 2; i <= 7; i++ {
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
