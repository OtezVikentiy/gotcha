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

// erroringGroupHook — фейковая реализация groupHook (duck-typing D3, см.
// evaluator.go): каждый метод возвращает заданную ошибку, остальные —
// nil/нулевые значения. Изолирует конкретную ветку groupGate/
// groupRootOpened/groupRootClosed от настоящего incidentgroup.Grouper
// (который в group_test.go используется для позитивных сценариев).
type erroringGroupHook struct {
	attachErr       error
	attachAttached  bool // R3b: forces attachedAsMember=true on Attach success (attachErr == nil)
	attachInforming bool
	onRootOpenedErr error
	onRootClosedErr error

	// rootIncidentErr/rootSource/rootIncidentID/rootFound — R3b: RootIncident
	// double, независимая от onRootOpenedErr — groupRootOpened зовёт
	// RootIncident ДО OnRootOpened, только когда attachedAsMember=true.
	rootIncidentErr error
	rootSource      string
	rootIncidentID  int64
	rootFound       bool

	// onRootOpenedCalls — считает вызовы OnRootOpened: R3b fail-safe требует
	// ТИХОГО возврата (без вызова OnRootOpened вовсе) при ошибке DownRoot/
	// RootIncident — счётчик ловит мутацию «продолжили как будто нашли
	// корень», которую notify-ассерты сами по себе не поймают (уведомление
	// свежего инцидента решается ДО groupRootOpened, gate независим).
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

// RootIncident — nil error/rootFound=false по умолчанию: безопасный no-op
// для тестов, не бьющих attachedAsMember=true ветку groupRootOpened.
// Настраиваемый (R3b): rootIncidentErr — инъекция ошибки резолва корня;
// rootSource/rootIncidentID/rootFound — успешный ответ на другой узел вида
// (кросс-видовой корень).
func (h *erroringGroupHook) RootIncident(ctx context.Context, rootKind string, rootID int64) (string, int64, int64, bool, bool, error) {
	if h.rootIncidentErr != nil {
		return "", 0, 0, false, false, h.rootIncidentErr
	}
	return h.rootSource, h.rootIncidentID, 0, false, h.rootFound, nil
}

// downRootStubChecker — depChecker с настраиваемым DownRoot; HasParent
// всегда false (эти тесты не про B5-отложенное уведомление, только про
// R3b-резолв фактического корня). Локальный для error-инъекции, в отличие
// от mockDepChecker (dep_test.go), у которого DownRoot жёстко not-found.
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

// captureErrorLog — подменяет slog.Default на текстовый handler уровня WARN
// (ловит и ERROR — Warn(4) < Error(8), Level — минимальный порог) и
// возвращает буфер + функцию восстановления (образец —
// TestProfileDeleteLogsWhenEmailReadFails, cover_profile_delete_test.go).
// Порог поднят с ERROR до WARN в R3b: root incident lookup failed
// (groupRootOpened, ошибка RootIncident) логируется через slog.Warn — та же
// severity, что и у пред-R3b «host evaluator: root incident lookup failed»
// (host-only лукап, который эта ветка заменила).
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestGroupGateAttachErrorStillNotifies — groupGate: ошибка Attach — fail-
// safe (см. докблок groupGate: «ведём себя как без D3, шумим»). Свежий
// silent-инцидент обязан уведомить сразу, как будто группы нет вовсе, а не
// молча потеряться под гейтом «grouped».
func TestGroupGateAttachErrorStillNotifies(t *testing.T) {
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

// TestGroupRootOpenedErrorLoggedNotFatal — groupRootOpened: ошибка
// ретро-присоединения (Р7) — не должна ронять открытие корня, только
// логируется (best-effort: уведомления УЖЕ ушли, состав — не критично).
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

// TestGroupRootClosedErrorLoggedNotFatal — groupRootClosed: ошибка закрытия
// группы (Р5) — sweep подстрахует (§4.4), но сам сбой обязан залогироваться,
// а закрытие инцидента и уведомление о восстановлении — не пострадать.
func TestGroupRootClosedErrorLoggedNotFatal(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid) // нужен RecoveryChannels — см. докблок seedAlertChannel
	h := seedEvalHost(t, pool, pid, "web-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = &erroringGroupHook{}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (open): %v", err)
	}

	// Хост снова жив — silent-инцидент должен закрыться следующим тиком.
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

// TestGroupRootOpenedDownRootErrorStillNotifies — groupRootOpened,
// attachedAsMember=true (R3b): ошибка e.Dep.DownRoot при резолве
// фактического корня каскада — тихий возврат БЕЗ вызова OnRootOpened
// (никакого «продолжим как будто корень — сам узел»), собственное
// уведомление свежего инцидента не подавлено, ошибка залогирована.
func TestGroupRootOpenedDownRootErrorStillNotifies(t *testing.T) {
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
	eval.Dep = &downRootStubChecker{err: errors.New("downroot boom")}
	// rootFound/rootSource/rootIncidentID: НЕ должны понадобиться при
	// корректном fail-safe (DownRoot error обязан вернуть ДО вызова
	// RootIncident) — настроены на успех намеренно, чтобы мутация «продолжим
	// как будто нашли корень вместо безопасного отката» была ловима через
	// onRootOpenedCalls, а не маскировалась их отсутствием.
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

// TestGroupRootOpenedRootIncidentErrorStillNotifies — groupRootOpened,
// attachedAsMember=true (R3b): DownRoot резолвит фактический (кросс-
// видовой) корень успешно, но e.IncidentGroups.RootIncident падает —
// тихий возврат БЕЗ вызова OnRootOpened, собственное уведомление свежего
// инцидента не подавлено, ошибка залогирована.
func TestGroupRootOpenedRootIncidentErrorStillNotifies(t *testing.T) {
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
