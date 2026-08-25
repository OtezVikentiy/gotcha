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

// RootIncident — not exercised by these error-branch tests (none drive
// groupRootOpened's attachedAsMember=true path); stubbed found=false so it
// stays a safe no-op default.
func (h *erroringGroupHook) RootIncident(ctx context.Context, rootKind string, rootID int64) (string, int64, int64, bool, bool, error) {
	return "", 0, 0, false, false, nil
}

// captureErrorLog — подменяет slog.Default на текстовый handler уровня
// ERROR и возвращает буфер + функцию восстановления (образец —
// TestProfileDeleteLogsWhenEmailReadFails, cover_profile_delete_test.go).
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
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
