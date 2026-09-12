package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

type mockDepChecker struct {
	hasParent bool
	err       error
	calls     int
}

func (m *mockDepChecker) HasParent(_ context.Context, _ string, _ int64) (bool, error) {
	m.calls++
	return m.hasParent, m.err
}

func (m *mockDepChecker) DownRoot(_ context.Context, _ string, _ int64) (string, int64, bool, error) {
	return "", 0, false, nil
}

func TestOpenUnackedExcludesSuppressed(t *testing.T) {
	pool, svc, pid, hostID := setupIncidentHost(t)
	ctx := context.Background()
	in, _, err := svc.Open(ctx, pid, hostID, "silent", 1, "", false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if list, err := svc.OpenUnacked(ctx); err != nil || len(list) != 1 {
		t.Fatalf("до подавления want 1, got %d (err=%v)", len(list), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET suppressed_by_dep=true WHERE id=$1`, in.ID); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if list, err := svc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("после подавления want 0, got %d (err=%v)", len(list), err)
	}
}

func TestEvaluatorDefersStep0WhenHostHasParent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	dep := &mockDepChecker{hasParent: true}
	eval.Dep = dep

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open even when host has a parent — только уведомление откладывается")
	}
	if dep.calls == 0 {
		t.Fatal("depChecker.HasParent не вызван")
	}
	if notifier.openedCount() != 0 {
		t.Errorf("opened notifications = %d, want 0 (step0 отложена планировщику B5)", notifier.openedCount())
	}
}

func TestEvaluatorSendsStep0WhenHostHasNoParent(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = &mockDepChecker{hasParent: false}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open")
	}
	if notifier.openedCount() != 1 {
		t.Errorf("opened notifications = %d, want 1 (без родителя step0 уходит сразу)", notifier.openedCount())
	}
}

func TestEvaluatorStep0FailSafeOnDepError(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = &mockDepChecker{err: errors.New("dep service unavailable")}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open despite dep checker error")
	}
	if notifier.openedCount() != 1 {
		t.Errorf("opened notifications = %d, want 1 (fail-safe: ошибка HasParent не должна глотать уведомление)", notifier.openedCount())
	}
}

func TestEvaluatorRecoveryNoLeakWhenSuppressed(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = &mockDepChecker{hasParent: true}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open")
	}

	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET suppressed_by_dep=true WHERE id=$1`, in.ID); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	var escCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM incident_escalations WHERE incident_source='host' AND incident_id=$1`, in.ID).
		Scan(&escCount); err != nil {
		t.Fatalf("count escalations: %v", err)
	}
	if escCount != 0 {
		t.Fatalf("incident_escalations count = %d, want 0 (step0 была отложена)", escCount)
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.50, time.Minute)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}

	_, open, err = incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Error("disk incident must be resolved after recovery to 0.50")
	}
	if notifier.resolvedCount() != 0 {
		t.Errorf("resolved notifications = %d, want 0 (пустой лог эскалации → notifyClose no-op)", notifier.resolvedCount())
	}
}
