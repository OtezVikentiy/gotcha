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

// erroringGroupHook — фейковая реализация groupHook (duck-typing D3, см.
// detector.go): каждый метод возвращает заданную ошибку. Изолирует
// конкретную error-ветку openIncident/settleHeldIncident/resolveIncident от
// настоящего incidentgroup.Grouper (используется для позитивных сценариев в
// group_test.go).
type erroringGroupHook struct {
	attachErr       error
	onRootOpenedErr error
	onRootClosedErr error
}

func (h *erroringGroupHook) Attach(ctx context.Context, source string, incidentID int64, nodeKind string, nodeID int64) (bool, bool, error) {
	if h.attachErr != nil {
		return false, false, h.attachErr
	}
	return false, false, nil
}

func (h *erroringGroupHook) OnRootOpened(ctx context.Context, rootSource string, rootIncidentID int64, rootNodeKind string, rootNodeID, projectID int64) error {
	return h.onRootOpenedErr
}

func (h *erroringGroupHook) OnRootClosed(ctx context.Context, rootSource string, rootIncidentID int64) error {
	return h.onRootClosedErr
}

// captureErrorLog — подменяет slog.Default на текстовый handler уровня
// ERROR (образец — cover_profile_delete_test.go/host/group_hook_error_test.go).
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestOpenIncidentGroupRootOpenedErrorLoggedNotFatal — openIncident:
// ошибка ретро-присоединения (Р7, OnRootOpened) не должна ронять открытие
// самого инцидента монитора, только логируется.
func TestOpenIncidentGroupRootOpenedErrorLoggedNotFatal(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	buf := captureErrorLog(t)

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	d.IncidentGroups = &erroringGroupHook{onRootOpenedErr: errors.New("retro boom")}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "boom", time.Now().UTC(), nil)

	assertOpenIncident(t, ctx, svc, mon.ID)
	if !strings.Contains(buf.String(), "group root opened failed") {
		t.Errorf("OnRootOpened error must be logged, got: %s", buf.String())
	}
}

// TestSettleHeldAttachErrorLeavesGroupIDNull — settleHeldIncident: ошибка
// Attach на B5-подавлении не должна отменять само подавление
// (SuppressedByDep), только состав группы — group_id остаётся NULL, а не
// молча "притворяется" присоединённым.
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
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second}
	d.IncidentGroups = &erroringGroupHook{attachErr: errors.New("attach boom")}
	now := time.Now().UTC()

	// Тик 1: ребёнок открывается, "down" придержан (HasParent).
	applyAndDetect(t, ctx, svc, d, child, "local", false, "boom", now, nil)
	// Тик 2: родитель down → settleHeldIncident подавляет; Attach падает.
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

// TestResolveIncidentGroupRootClosedErrorLoggedNotFatal — resolveIncident:
// ошибка закрытия группы (Р5, OnRootClosed) не должна мешать закрытию
// самого инцидента монитора и recovery-уведомлению.
func TestResolveIncidentGroupRootClosedErrorLoggedNotFatal(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	mon := createMonitor(t, svc, pid, 1, 1) // recovery_threshold=1 — один "ok" уже разрешает

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
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
