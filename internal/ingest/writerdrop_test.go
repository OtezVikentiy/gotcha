package ingest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

type fakeProjectResolver struct {
	mu       sync.Mutex
	projects map[int64]org.Project
	err      error
	calls    int
}

func newFakeProjectResolver() *fakeProjectResolver {
	return &fakeProjectResolver{projects: map[int64]org.Project{}}
}

func (f *fakeProjectResolver) Resolve(_ context.Context, projectID int64) (org.Project, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return org.Project{}, f.err
	}
	p, ok := f.projects[projectID]
	if !ok {
		return org.Project{}, errors.New("project not found")
	}
	return p, nil
}

type fakeRefunder struct {
	mu       sync.Mutex
	metrics  map[int64]int64
	profiles map[int64]int64
}

func newFakeRefunder() *fakeRefunder {
	return &fakeRefunder{metrics: map[int64]int64{}, profiles: map[int64]int64{}}
}

func (f *fakeRefunder) RefundMetrics(ctx context.Context, orgID int64, _ time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metrics[orgID] += n
	return nil
}

func (f *fakeRefunder) RefundProfiles(ctx context.Context, orgID int64, _ time.Time, n int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[orgID] += n
	return nil
}

func (f *fakeRefunder) metricsFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.metrics[orgID]
}

func (f *fakeRefunder) profilesFor(orgID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profiles[orgID]
}

// add() не должен ходить в PG сам — иначе стопорил бы следующий флаш писателя;
// проверяем, что до flush() ничего не уходит ни в счётчик, ни в возврат квоты.
func TestWriterDropAttributorAggregatesUntilFlush(t *testing.T) {
	resolver := newFakeProjectResolver()
	resolver.projects[10] = org.Project{ID: 10, OrgID: 99}
	dc := newFakeDropCounter()
	rf := newFakeRefunder()
	a := NewWriterDropAttributor(resolver, dc, rf)

	a.CountDroppedMetrics(10, 3)
	a.CountDroppedMetrics(10, 2)
	if dc.metricsCalls != 0 {
		t.Fatalf("IncDroppedMetrics вызван до флаша: %d раз, want 0 — агрегация должна копиться в памяти", dc.metricsCalls)
	}
	if got := rf.metricsFor(99); got != 0 {
		t.Fatalf("RefundMetrics вызван до флаша для org 99: %d, want 0", got)
	}

	a.flush(context.Background())
	if got := dc.metricsFor(99); got != 5 {
		t.Errorf("dropped metrics для org 99 после флаша = %d, want 5 (3+2, резолв project 10 → org 99)", got)
	}
	if got := rf.metricsFor(99); got != 5 {
		t.Errorf("возврат квоты метрик для org 99 после флаша = %d, want 5", got)
	}

	// агрегат обязан обнуляться при флаше — иначе окно задваивалось бы на каждый следующий тик.
	a.flush(context.Background())
	if got := dc.metricsFor(99); got != 5 {
		t.Errorf("повторный флаш изменил счётчик дропов: got %d, want 5", got)
	}
	if got := rf.metricsFor(99); got != 5 {
		t.Errorf("повторный флаш изменил возврат квоты: got %d, want 5", got)
	}
}

func TestWriterDropAttributorProfilesCountAndRefund(t *testing.T) {
	resolver := newFakeProjectResolver()
	resolver.projects[20] = org.Project{ID: 20, OrgID: 7}
	dc := newFakeDropCounter()
	rf := newFakeRefunder()
	a := NewWriterDropAttributor(resolver, dc, rf)

	a.CountDroppedProfiles(20, 4)
	a.flush(context.Background())

	if got := dc.profilesFor(7); got != 4 {
		t.Errorf("dropped profiles для org 7 = %d, want 4", got)
	}
	if got := rf.profilesFor(7); got != 4 {
		t.Errorf("возврат квоты профилей для org 7 = %d, want 4", got)
	}
	// метрики и профили — разные классы дропа, не должны смешиваться в одном ведре.
	if got := dc.metricsFor(7); got != 0 {
		t.Errorf("IncDroppedMetrics задет профильным дропом: %d, want 0", got)
	}
}

// Резолв не удался (проект удалён/PG недоступна) — потеря не должна повиснуть
// в памяти навсегда и не должна запаниковать; факт логируется (см. flush).
func TestWriterDropAttributorResolveFailureDropsWindowNotRetried(t *testing.T) {
	resolver := newFakeProjectResolver()
	resolver.err = errors.New("pg unavailable")
	dc := newFakeDropCounter()
	rf := newFakeRefunder()
	a := NewWriterDropAttributor(resolver, dc, rf)

	a.CountDroppedMetrics(10, 3)
	a.flush(context.Background())

	if dc.metricsCalls != 0 {
		t.Errorf("IncDroppedMetrics вызван при неудачном резолве: %d раз, want 0", dc.metricsCalls)
	}
	if got := rf.metricsFor(99); got != 0 {
		t.Errorf("RefundMetrics вызван при неудачном резолве: %d, want 0", got)
	}
	if calls := resolver.calls; calls != 1 {
		t.Fatalf("резолвер вызван %d раз, want 1", calls)
	}

	// агрегат уже слит (best-effort) — следующий флаш не должен повторно резолвить то же окно.
	a.flush(context.Background())
	if calls := resolver.calls; calls != 1 {
		t.Errorf("повторный флаш заново обратился к резолверу: %d раз, want 1 — потерянное окно не ретраится", calls)
	}
}

func TestWriterDropAttributorIgnoresZeroProjectAndNonPositiveN(t *testing.T) {
	resolver := newFakeProjectResolver()
	dc := newFakeDropCounter()
	rf := newFakeRefunder()
	a := NewWriterDropAttributor(resolver, dc, rf)

	a.CountDroppedMetrics(0, 5)
	a.CountDroppedProfiles(10, 0)
	a.flush(context.Background())

	if resolver.calls != 0 {
		t.Errorf("резолвер вызван %d раз для project_id=0/n<=0, want 0", resolver.calls)
	}
	if dc.metricsCalls != 0 || dc.profilesCalls != 0 {
		t.Errorf("счётчик вызван для дропа без project_id/с n<=0: metrics=%d profiles=%d, want 0/0",
			dc.metricsCalls, dc.profilesCalls)
	}
}

// настоящий цикл Run/Close, не прямой вызов flush(): Close обязан слить
// накопленное окно перед остановкой, а не просто оборвать тикер.
func TestWriterDropAttributorRunCloseFlushesOnStop(t *testing.T) {
	resolver := newFakeProjectResolver()
	resolver.projects[10] = org.Project{ID: 10, OrgID: 99}
	dc := newFakeDropCounter()
	rf := newFakeRefunder()
	a := NewWriterDropAttributor(resolver, dc, rf)
	a.flushInterval = time.Hour // тик заведомо не успеет сработать за время теста

	go a.Run()
	a.CountDroppedMetrics(10, 4)
	a.Close()

	if got := dc.metricsFor(99); got != 4 {
		t.Errorf("dropped metrics после Close = %d, want 4 — Close обязан слить последнее окно", got)
	}
	if got := rf.metricsFor(99); got != 4 {
		t.Errorf("возврат квоты после Close = %d, want 4", got)
	}
}

type failingCounter struct{ err error }

func (f *failingCounter) IncDroppedEvents(context.Context, int64, time.Time, int64) error { return nil }
func (f *failingCounter) IncDroppedTransactions(context.Context, int64, time.Time, int64) error {
	return nil
}
func (f *failingCounter) IncDroppedMetrics(context.Context, int64, time.Time, int64) error {
	return f.err
}
func (f *failingCounter) IncDroppedProfiles(context.Context, int64, time.Time, int64) error {
	return f.err
}
func (f *failingCounter) IncDroppedLogs(context.Context, int64, time.Time, int64) error { return nil }

type failingRefunder struct{ err error }

func (f *failingRefunder) RefundMetrics(context.Context, int64, time.Time, int64) error {
	return f.err
}
func (f *failingRefunder) RefundProfiles(context.Context, int64, time.Time, int64) error {
	return f.err
}

// провал одной стороны не должен глушить другую — учёт и возврат квоты
// независимы, report() обязан звать обе даже если первая упала.
func TestWriterDropAttributorReportHandlesCounterAndRefundFailuresIndependently(t *testing.T) {
	resolver := newFakeProjectResolver()
	resolver.projects[10] = org.Project{ID: 10, OrgID: 99}

	failCounter := &failingCounter{err: errors.New("inc failed")}
	rf := newFakeRefunder()
	a := NewWriterDropAttributor(resolver, failCounter, rf)
	a.CountDroppedMetrics(10, 3)
	a.flush(context.Background())
	if got := rf.metricsFor(99); got != 3 {
		t.Errorf("Refund не выполнен после неудачного Inc: got %d, want 3", got)
	}

	dc := newFakeDropCounter()
	failRefund := &failingRefunder{err: errors.New("refund failed")}
	a2 := NewWriterDropAttributor(resolver, dc, failRefund)
	a2.CountDroppedMetrics(10, 5)
	a2.flush(context.Background())
	if got := dc.metricsFor(99); got != 5 {
		t.Errorf("Inc не выполнен после неудачного Refund: got %d, want 5", got)
	}
}
