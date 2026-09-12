package host_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type erroringGroupHook struct {
	attachErr       error
	attachAttached  bool
	attachInforming bool
	onRootOpenedErr error
	onRootClosedErr error

	rootIncidentErr error
	rootSource      string
	rootIncidentID  int64
	rootFound       bool

	onRootOpenedCalls int
}

func (h *erroringGroupHook) Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (bool, bool, error) {
	if h.attachErr != nil {
		return false, false, h.attachErr
	}
	return h.attachAttached, h.attachInforming, nil
}

func (h *erroringGroupHook) OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error {
	h.onRootOpenedCalls++
	return h.onRootOpenedErr
}

func (h *erroringGroupHook) OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error {
	return h.onRootClosedErr
}

func (h *erroringGroupHook) RootIncident(ctx context.Context, rootKind string, rootID int64) (string, int64, int64, bool, bool, error) {
	if h.rootIncidentErr != nil {
		return "", 0, 0, false, false, h.rootIncidentErr
	}
	return h.rootSource, h.rootIncidentID, 0, false, h.rootFound, nil
}

type downRootStubChecker struct {
	rootKind string
	rootID   int64
	found    bool
	err      error
}

func (c *downRootStubChecker) HasParent(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (c *downRootStubChecker) DownRoot(context.Context, string, int64) (string, int64, bool, error) {
	return c.rootKind, c.rootID, c.found, c.err
}

func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestGroupGateAttachErrorStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	buf := captureErrorLog(t)

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = &erroringGroupHook{attachErr: errors.New("attach boom")}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("silent incident must open despite Attach error")
	}
	if len(notifier.opened) != 1 || notifier.opened[0].ID != in.ID {
		t.Fatalf("Attach error must not suppress the open notification (fail-noisy): opened=%v", notifier.opened)
	}
	if !strings.Contains(buf.String(), "group attach failed") {
		t.Errorf("Attach error must be logged, got: %s", buf.String())
	}
}

func TestGroupRootOpenedErrorLoggedNotFatal(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	buf := captureErrorLog(t)

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = &erroringGroupHook{onRootOpenedErr: errors.New("retro boom")}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("silent incident must still open despite OnRootOpened error")
	}
	if !strings.Contains(buf.String(), "group root opened failed") {
		t.Errorf("OnRootOpened error must be logged, got: %s", buf.String())
	}
}

func TestGroupRootClosedErrorLoggedNotFatal(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = &erroringGroupHook{}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (open): %v", err)
	}

	setHostLastSeen(t, pool, h.ID, time.Now().UTC())
	eval.IncidentGroups = &erroringGroupHook{onRootClosedErr: errors.New("close boom")}
	buf := captureErrorLog(t)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (close): %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Fatal("silent incident must still resolve despite OnRootClosed error")
	}
	if len(notifier.resolved) != 1 {
		t.Fatalf("OnRootClosed error must not suppress the recovery notification: resolved=%v", notifier.resolved)
	}
	if !strings.Contains(buf.String(), "group root closed failed") {
		t.Errorf("OnRootClosed error must be logged, got: %s", buf.String())
	}
}

func TestGroupRootOpenedDownRootErrorStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	buf := captureErrorLog(t)

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = &downRootStubChecker{err: errors.New("downroot boom")}
	hook := &erroringGroupHook{attachAttached: true, rootFound: true, rootSource: "host", rootIncidentID: 999}
	eval.IncidentGroups = hook

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("silent incident must open despite DownRoot error")
	}
	if len(notifier.opened) != 1 || notifier.opened[0].ID != in.ID {
		t.Fatalf("DownRoot error must not suppress the open notification (fail-noisy): opened=%v", notifier.opened)
	}
	if hook.onRootOpenedCalls != 0 {
		t.Errorf("OnRootOpened calls = %d, want 0: DownRoot error must skip retro-attach entirely, not fall back to a bogus root", hook.onRootOpenedCalls)
	}
	if !strings.Contains(buf.String(), "down root lookup failed") {
		t.Errorf("DownRoot error must be logged, got: %s", buf.String())
	}
}

func TestGroupRootOpenedRootIncidentErrorStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()
	buf := captureErrorLog(t)

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = &downRootStubChecker{rootKind: "monitor", rootID: 42, found: true}
	hook := &erroringGroupHook{attachAttached: true, rootIncidentErr: errors.New("rootinc boom")}
	eval.IncidentGroups = hook

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("silent incident must open despite RootIncident error")
	}
	if len(notifier.opened) != 1 || notifier.opened[0].ID != in.ID {
		t.Fatalf("RootIncident error must not suppress the open notification (fail-noisy): opened=%v", notifier.opened)
	}
	if hook.onRootOpenedCalls != 0 {
		t.Errorf("OnRootOpened calls = %d, want 0: RootIncident error must skip retro-attach entirely, not fall back to a bogus root", hook.onRootOpenedCalls)
	}
	if !strings.Contains(buf.String(), "root incident lookup failed") {
		t.Errorf("RootIncident error must be logged, got: %s", buf.String())
	}
}
