package uptime_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

type erroringGroupHook struct {
	attachErr       error
	onRootOpenedErr error
	onRootClosedErr error

	rootIncidentErr error
	rootSource      string
	rootIncidentID  int64
	rootFound       bool

	onRootOpenedCalls  int
	lastRootSource     string
	lastRootIncidentID int64
	lastRootNodeKind   string
	lastRootNodeID     int64
}

func (h *erroringGroupHook) Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (bool, bool, error) {
	if h.attachErr != nil {
		return false, false, h.attachErr
	}
	return false, false, nil
}

func (h *erroringGroupHook) OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error {
	h.onRootOpenedCalls++
	h.lastRootSource, h.lastRootIncidentID, h.lastRootNodeKind, h.lastRootNodeID = rootSource, rootIncidentID, rootNodeKind, rootNodeID
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

func (c *downRootStubChecker) ParentDown(context.Context, string, int64) (bool, error) {
	return false, nil
}

func (c *downRootStubChecker) DownRoot(context.Context, string, int64) (string, int64, bool, error) {
	return c.rootKind, c.rootID, c.found, c.err
}

// уровень WARN, не ERROR: Level — минимальный порог (Warn(4) < Error(8)),
// так что ловит и Warn-, и Error-логи.
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestOpenIncidentGroupRootOpenedErrorLoggedNotFatal(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buf := captureErrorLog(t)

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	d.IncidentGroups = &erroringGroupHook{onRootOpenedErr: errors.New("retro boom")}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", time.Now().UTC(), nil)

	assertOpenIncident(t, ctx, svc, mon.ID)
	if !strings.Contains(buf.String(), "group root opened failed") {
		t.Errorf("OnRootOpened error must be logged, got: %s", buf.String())
	}
}

func TestSettleHeldAttachErrorLeavesGroupIDNull(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buf := captureErrorLog(t)

	pid := newProject(t, pool)
	parent := createMonitor(t, svc, pid, 1, 1)
	child := createMonitor(t, svc, pid, 1, 1)

	if _, created, err := svc.OpenIncident(ctx, parent.ID, "root down", []string{"local"}, false); err != nil || !created {
		t.Fatalf("open parent incident: created=%v err=%v", created, err)
	}

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: true}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	d.IncidentGroups = &erroringGroupHook{attachErr: errors.New("attach boom")}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, child, "local", false, "boom", now, nil)
	applyAndDetect(t, ctx, svc, d, child, "local", false, "boom", now.Add(time.Second), nil)

	inc := assertOpenIncident(t, ctx, svc, child.ID)
	if !inc.SuppressedByDep {
		t.Fatal("Attach error must not cancel the B5 suppression itself: SuppressedByDep = false, want true")
	}
	gid := readUptimeGroupID(t, pool, inc.ID)
	if gid != nil {
		t.Fatalf("Attach error must leave the incident ungrouped, got group_id=%d", *gid)
	}
	if !strings.Contains(buf.String(), "group attach failed") {
		t.Errorf("Attach error must be logged, got: %s", buf.String())
	}
}

func TestResolveIncidentGroupRootClosedErrorLoggedNotFatal(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	d.IncidentGroups = &erroringGroupHook{}
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", now, nil)
	assertOpenIncident(t, ctx, svc, mon.ID)
	backdateIncidentStart(t, ctx, pool, mon.ID)

	buf := captureErrorLog(t)
	d.IncidentGroups = &erroringGroupHook{onRootClosedErr: errors.New("close boom")}
	applyAndDetect(t, ctx, svc, d, mon, "local", true, "", now.Add(time.Second), nil)

	assertNoOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.kindEvents("up")) != 1 {
		t.Fatalf("OnRootClosed error must not suppress the recovery notification: up events = %d", len(notifier.kindEvents("up")))
	}
	if !strings.Contains(buf.String(), "group root closed failed") {
		t.Errorf("OnRootClosed error must be logged, got: %s", buf.String())
	}
}

func TestOpenIncidentDownRootErrorFallsBackToSelfAndNotifies(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buf := captureErrorLog(t)

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: &downRootStubChecker{err: errors.New("downroot boom")}, Pool: pool}
	hook := &erroringGroupHook{}
	d.IncidentGroups = hook

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", time.Now().UTC(), nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.kindEvents("down")) != 1 {
		t.Fatalf("DownRoot error must not suppress the open notification (fail-noisy): down events = %d", len(notifier.kindEvents("down")))
	}
	if hook.onRootOpenedCalls != 1 {
		t.Fatalf("OnRootOpened calls = %d, want 1: DownRoot error must still trigger the retro-scan, just rooted at the monitor itself", hook.onRootOpenedCalls)
	}
	if hook.lastRootSource != "uptime" || hook.lastRootIncidentID != inc.ID || hook.lastRootNodeKind != "monitor" || hook.lastRootNodeID != mon.ID {
		t.Errorf("OnRootOpened root = %s/%d (node %s/%d), want self uptime/%d (monitor/%d): DownRoot error must not fabricate a different root",
			hook.lastRootSource, hook.lastRootIncidentID, hook.lastRootNodeKind, hook.lastRootNodeID, inc.ID, mon.ID)
	}
	if !strings.Contains(buf.String(), "down root lookup failed") {
		t.Errorf("DownRoot error must be logged, got: %s", buf.String())
	}
}

func TestOpenIncidentRootIncidentErrorFallsBackToSelfAndNotifies(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buf := captureErrorLog(t)

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: &downRootStubChecker{rootKind: "host", rootID: 777, found: true}, Pool: pool}
	hook := &erroringGroupHook{rootIncidentErr: errors.New("rootinc boom")}
	d.IncidentGroups = hook

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", time.Now().UTC(), nil)

	inc := assertOpenIncident(t, ctx, svc, mon.ID)
	if len(notifier.kindEvents("down")) != 1 {
		t.Fatalf("RootIncident error must not suppress the open notification (fail-noisy): down events = %d", len(notifier.kindEvents("down")))
	}
	if hook.onRootOpenedCalls != 1 {
		t.Fatalf("OnRootOpened calls = %d, want 1: RootIncident error must still trigger the retro-scan, just rooted at the monitor itself", hook.onRootOpenedCalls)
	}
	if hook.lastRootSource != "uptime" || hook.lastRootIncidentID != inc.ID || hook.lastRootNodeKind != "monitor" || hook.lastRootNodeID != mon.ID {
		t.Errorf("OnRootOpened root = %s/%d (node %s/%d), want self uptime/%d (monitor/%d): RootIncident error must not fabricate the (host,777) root DownRoot resolved",
			hook.lastRootSource, hook.lastRootIncidentID, hook.lastRootNodeKind, hook.lastRootNodeID, inc.ID, mon.ID)
	}
	if !strings.Contains(buf.String(), "root incident lookup failed") {
		t.Errorf("RootIncident error must be logged, got: %s", buf.String())
	}
}
