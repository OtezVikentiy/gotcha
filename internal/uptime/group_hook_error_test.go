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

	// rootIncidentErr/rootSource/rootIncidentID/rootFound — R3b: RootIncident
	// double, инъекция ошибки резолва кросс-видового корня в openIncident.
	rootIncidentErr error
	rootSource      string
	rootIncidentID  int64
	rootFound       bool

	// onRootOpenedCalls/lastRoot* — R3b: записывает КАЖДЫЙ вызов
	// OnRootOpened с его аргументами. Fail-safe openIncident при ошибке
	// DownRoot/RootIncident обязан вызвать OnRootOpened с самим монитором
	// как корнем (откат к поведению до R3b), а не молча пропустить вызов и
	// не подставить фабрикованный чужой корень — счётчик и последние
	// аргументы различают все три исхода.
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

// RootIncident — nil error/rootFound=false по умолчанию: безопасный no-op
// для тестов, не бьющих кросс-видовую ветку openIncident. Настраиваемый
// (R3b): rootIncidentErr — инъекция ошибки резолва корня; rootSource/
// rootIncidentID/rootFound — успешный ответ (не используется этими
// error-тестами, но держит форму симметрично host/group_hook_error_test.go).
func (h *erroringGroupHook) RootIncident(ctx context.Context, rootKind string, rootID int64) (string, int64, int64, bool, bool, error) {
	if h.rootIncidentErr != nil {
		return "", 0, 0, false, false, h.rootIncidentErr
	}
	return h.rootSource, h.rootIncidentID, 0, false, h.rootFound, nil
}

// downRootStubChecker — depChecker с настраиваемым DownRoot; HasParent/
// ParentDown всегда false (эти тесты не про B5-отложенное уведомление,
// только про R3b-резолв фактического корня в openIncident). Локальный для
// error-инъекции, в отличие от fakeDepChecker (detector_test.go), у
// которого DownRoot жёстко not-found.
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

// captureErrorLog — подменяет slog.Default на текстовый handler уровня WARN
// (ловит и ERROR — Warn(4) < Error(8), Level — минимальный порог; образец —
// cover_profile_delete_test.go/host/group_hook_error_test.go). Порог поднят
// с ERROR до WARN в R3b: root incident lookup failed (openIncident, ошибка
// RootIncident) логируется через slog.Warn.
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
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
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
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
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
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

// TestOpenIncidentDownRootErrorFallsBackToSelfAndNotifies — openIncident
// (R3b): ошибка d.Dep.DownRoot при резолве фактического корня каскада —
// fail-safe откат на САМ монитор как корень (ровно поведение ДО R3b), а не
// молчание (OnRootOpened всё равно зовётся — ретро-перебор проекта не
// должен пропускаться из-за временной ошибки Suppressor'а) и не
// фабрикованный чужой корень. Открытие/уведомление монитора не затронуты
// вовсе — это независимый путь (гейт hasParent).
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

// TestOpenIncidentRootIncidentErrorFallsBackToSelfAndNotifies — openIncident
// (R3b): DownRoot резолвит фактический (кросс-видовой) корень успешно, но
// e.IncidentGroups.RootIncident падает — тот же fail-safe откат на сам
// монитор как корень, OnRootOpened всё равно зовётся, открытие/уведомление
// монитора не затронуты.
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
