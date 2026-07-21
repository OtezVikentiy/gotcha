package metric_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// TestEvaluatorRunLifecycle: с крошечным Interval цикл Run должен хотя бы раз
// тикнуть (Tick → ListEnabled на реальном, пустом пуле) и завершиться по
// отмене ctx — покрывает обе ветки select (tick.C и ctx.Done).
func TestEvaluatorRunLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	eval := &metric.Evaluator{
		Rules:    metric.NewRuleService(pool),
		Interval: 2 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		eval.Run(ctx)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Evaluator.Run did not return after ctx cancel")
	}
}

// TestEvaluatorRunDefaultInterval: Interval<=0 → берётся дефолт (60s), тика мы
// не дождёмся, но ветка выбора дефолта и выход по ctx.Done покрываются.
func TestEvaluatorRunDefaultInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	eval := &metric.Evaluator{Rules: metric.NewRuleService(pool), Interval: 0}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		eval.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Evaluator.Run did not return after ctx cancel")
	}
}

// TestEvaluatorBumpAndNilNotifier: без Notifier открытие инцидента не должно
// падать (notify() рано выходит при Notifier==nil), а повторный тик с более
// экстремальным значением при уже открытом инциденте идёт по ветке d.Bump —
// обновляет current/peak, не открывая новый и не закрывая.
func TestEvaluatorBumpAndNilNotifier(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	projectID := seedProject(t, pool)

	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}

	eval := &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: nil, // notify() должен рано выйти, не паникуя
		Interval: time.Hour,
	}

	// Первый тик: avg 150 > 100 → инцидент открыт, peak≈150.
	seedMetricGauge(t, ch, projectID, "cpu", 140, time.Minute)
	seedMetricGauge(t, ch, projectID, "cpu", 160, 2*time.Minute)
	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil || !open {
		t.Fatalf("incident must be open after breach (err=%v)", err)
	}
	if in.PeakValue < 149 || in.PeakValue > 151 {
		t.Fatalf("peak after open = %v, want ≈150", in.PeakValue)
	}

	// Второй тик: значение выросло до 200 (всё ещё нарушение при открытом
	// инциденте) → ветка Bump, peak обновляется до 200.
	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedMetricGauge(t, ch, projectID, "cpu", 200, time.Minute)
	eval.Tick(ctx)

	in2, open2, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil || !open2 {
		t.Fatalf("incident must stay open after bump (err=%v)", err)
	}
	if in2.ID != in.ID {
		t.Fatalf("bump opened a new incident: %d vs %d", in2.ID, in.ID)
	}
	if in2.PeakValue != 200 {
		t.Fatalf("peak after bump = %v, want 200", in2.PeakValue)
	}
	if in2.CurrentValue != 200 {
		t.Fatalf("current after bump = %v, want 200", in2.CurrentValue)
	}
}

// TestIncidentBumpNotFound: Bump по несуществующему (или закрытому) инциденту →
// ErrIncidentNotFound (RowsAffected==0).
func TestIncidentBumpNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := metric.NewIncidentService(pool)

	err := svc.Bump(context.Background(), 999999, 1, 1)
	if !errors.Is(err, metric.ErrIncidentNotFound) {
		t.Fatalf("Bump(missing) = %v, want ErrIncidentNotFound", err)
	}
}

// TestIncidentBumpCancelledCtx: Bump на отменённом ctx — пул возвращает ошибку
// до выполнения SQL, покрывает ветку `if err != nil` в Bump.
func TestIncidentBumpCancelledCtx(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	svc := metric.NewIncidentService(pool)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Bump(ctx, 1, 1, 1); err == nil {
		t.Fatal("Bump on cancelled ctx: got nil, want DB error")
	} else if errors.Is(err, metric.ErrIncidentNotFound) {
		t.Fatalf("Bump on cancelled ctx = %v, want DB error not ErrIncidentNotFound", err)
	}
}
