package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// mockDepChecker — фейковая реализация локального depChecker (см.
// evaluator.go) для тестов подавления по дереву зависимостей (B5, T4).
// Duck-typing: host.Evaluator.Dep — неэкспортируемый тип интерфейса, но поле
// Dep экспортировано, и Go проверяет соответствие структурно — явно ссылаться
// на имя типа depChecker снаружи пакета не нужно.
type mockDepChecker struct {
	hasParent bool
	err       error
	calls     int
}

func (m *mockDepChecker) HasParent(_ context.Context, _ string, _ int64) (bool, error) {
	m.calls++
	return m.hasParent, m.err
}

// DownRoot — заглушка (R3, W25): тесты этого файла бьют по HasParent/step0,
// groupRootOpened их не касается (IncidentGroups не задан), но mockDepChecker
// обязан структурно закрывать depChecker целиком.
func (m *mockDepChecker) DownRoot(_ context.Context, _ string, _ int64) (string, int64, bool, error) {
	return "", 0, false, nil
}

// TestOpenUnackedExcludesSuppressed — планировщик эскалации (T7) не должен
// видеть инциденты, подавленные деп-планировщиком (T5): OpenUnacked
// фильтрует их по suppressed_by_dep, как требует Step 1 брифа Task 4.
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
	// Флаг ставит Suppressor/планировщик (T5); в этом стор-тесте выставляем
	// сырым SQL, как и в брифе.
	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET suppressed_by_dep=true WHERE id=$1`, in.ID); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	if list, err := svc.OpenUnacked(ctx); err != nil || len(list) != 0 {
		t.Fatalf("после подавления want 0, got %d (err=%v)", len(list), err)
	}
}

// TestEvaluatorDefersStep0WhenHostHasParent — хост с задекларированным
// родителем (HasParent→true) всё равно открывает диск-инцидент, но
// синхронная ступень 0 НЕ уходит — её досылает планировщик деп-подавления
// (T5), а не Evaluator напрямую.
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

// TestEvaluatorSendsStep0WhenHostHasNoParent — зеркало предыдущего теста:
// без родителя (HasParent→false) поведение не меняется — ступень 0 уходит
// синхронно, как до B5.
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

// TestEvaluatorStep0FailSafeOnDepError — сбой depChecker.HasParent не должен
// глушить уведомление (§7.7, MINOR-7 брифа): инцидент всё равно открывается
// И ступень 0 уходит — fail-safe трактует ошибку как «родителя нет».
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

// TestEvaluatorRecoveryNoLeakWhenSuppressed (★ §7.3, MAJOR-2) — host-инцидент,
// у которого step0 был отложен (родитель есть → в incident_escalations по
// нему НЕТ записей), и который затем помечен suppressed_by_dep=true
// планировщиком (T5), при восстановлении не шлёт NotifyRecovery: RecoveryChannels
// не находит адресатов (пустой лог эскалации), notifyClose — no-op. Это
// подтверждает «бесплатный» анти-шторм на восстановлении для host — без
// какого-либо дополнительного кода в notifyClose.
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
	eval.Dep = &mockDepChecker{hasParent: true} // step0 отложена → ничего не залогировано

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

	// Флаг ставит Suppressor/планировщик (T5); в этом тесте — сырым SQL.
	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET suppressed_by_dep=true WHERE id=$1`, in.ID); err != nil {
		t.Fatalf("set flag: %v", err)
	}

	// Предпосылка сценария: записей эскалации по инциденту действительно нет.
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
