package escalation_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/db"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// queryCountTracer считает запросы, чей текст содержит substr — используется
// вместо мока PolicyStore (он бьёт напрямую по *pgxpool.Pool, интерфейса нет).
type queryCountTracer struct {
	substr string
	calls  atomic.Int64
}

func (tr *queryCountTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, tr.substr) {
		tr.calls.Add(1)
	}
	return ctx
}

func (tr *queryCountTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// failNthQueryTracer как queryCountTracer, но у occurrence-го совпадающего
// запроса (считая с 1) подменяет контекст на уже отменённый — падает как настоящий транзиентный сбой PG.
type failNthQueryTracer struct {
	substr string
	failAt int64
	seen   atomic.Int64
}

func (tr *failNthQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, tr.substr) {
		return ctx
	}
	n := tr.seen.Add(1)
	if tr.failAt != 0 && n == tr.failAt {
		failedCtx, cancel := context.WithCancel(ctx)
		cancel()
		return failedCtx
	}
	return ctx
}

func (tr *failNthQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func tracedPool(t *testing.T, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	dsn := testenv.PostgresDSN(t)
	if err := db.MigratePG(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type countingMaint struct {
	calls atomic.Int64
}

func (m *countingMaint) InMaintenance(context.Context, int64, time.Time) (bool, error) {
	m.calls.Add(1)
	return false, nil
}

// errOnceMaint падает ровно на первом вызове (транзиентный сбой), дальше отвечает нормально.
type errOnceMaint struct {
	calls atomic.Int64
}

func (m *errOnceMaint) InMaintenance(context.Context, int64, time.Time) (bool, error) {
	if m.calls.Add(1) == 1 {
		return false, errors.New("transient maintenance check failure")
	}
	return false, nil
}

// Пять инцидентов одного проекта, две важности со своими лесенками.
func TestSchedulerTickCachesMaintenancePerProjectAndLadderPerSeverity(t *testing.T) {
	tracer := &queryCountTracer{substr: "escalation_steps"}
	pool := tracedPool(t, tracer)
	ctx := context.Background()
	pid := newProject(t, pool)
	cCrit := newChannel(t, pool, pid, true)
	cWarn := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityCritical, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{cCrit}},
	})
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{cWarn}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("metric")
	wantChannel := map[int64]int64{}
	for i := int64(1); i <= 3; i++ {
		src.add(escalation.PendingIncident{
			ID: i, ProjectID: pid, StartedAt: now.Add(-time.Minute),
			Severity: escalation.SeverityCritical, EscalationLevel: 0,
		})
		wantChannel[i] = cCrit
	}
	for i := int64(4); i <= 5; i++ {
		src.add(escalation.PendingIncident{
			ID: i, ProjectID: pid, StartedAt: now.Add(-time.Minute),
			Severity: escalation.SeverityWarning, EscalationLevel: 0,
		})
		wantChannel[i] = cWarn
	}
	notifier := &fakeNotifier{}
	maint := &countingMaint{}
	tracer.calls.Store(0) // setLadder выше сам пишет в escalation_steps — считаем только запросы тика

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    maint,
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 5 {
		t.Fatalf("NotifyStep calls = %d, want 5 (все пять инцидентов должны эскалироваться)", notifier.callCount())
	}
	if got := maint.calls.Load(); got != 1 {
		t.Errorf("InMaintenance calls = %d, want 1 (один проект — один запрос на тик, не на инцидент)", got)
	}
	if got := tracer.calls.Load(); got != 2 {
		t.Errorf("escalation_steps queries = %d, want 2 (по одному на пару проект+важность, не на инцидент и не один общий на проект)", got)
	}
	for _, call := range notifier.allCalls() {
		want := wantChannel[call.incidentID]
		if len(call.channelIDs) != 1 || call.channelIDs[0] != want {
			t.Errorf("инцидент %d получил каналы %v, want [%d] — кеш лесенки перепутал важности", call.incidentID, call.channelIDs, want)
		}
	}
}

// Транзиентная ошибка на первом инциденте не должна подменяться кешем для второго.
func TestSchedulerTickMaintenanceErrorNotCachedAcrossIncidents(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("metric")
	src.add(escalation.PendingIncident{
		ID: 1, ProjectID: pid, StartedAt: now.Add(-time.Minute),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.add(escalation.PendingIncident{
		ID: 2, ProjectID: pid, StartedAt: now.Add(-time.Minute),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}
	maint := &errOnceMaint{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    maint,
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if got := maint.calls.Load(); got != 2 {
		t.Errorf("InMaintenance calls = %d, want 2 (ошибка первого вызова не должна кешироваться и блокировать повтор для второго инцидента)", got)
	}
	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (первый инцидент упал на ошибке проверки окна и корректно не эскалирован в этом тике)", notifier.callCount())
	}
	if call := notifier.last(); call.incidentID != 2 {
		t.Errorf("уведомлён инцидент %d, want 2", call.incidentID)
	}
}

// Та же гарантия для лесенки политики (Policy — конкретный тип, отсюда tracer вместо фейка).
func TestSchedulerTickLadderErrorNotCachedAcrossIncidents(t *testing.T) {
	tracer := &failNthQueryTracer{substr: "escalation_steps"}
	pool := tracedPool(t, tracer)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("metric")
	src.add(escalation.PendingIncident{
		ID: 1, ProjectID: pid, StartedAt: now.Add(-time.Minute),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.add(escalation.PendingIncident{
		ID: 2, ProjectID: pid, StartedAt: now.Add(-time.Minute),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}
	tracer.seen.Store(0) // setLadder выше сам пишет в escalation_steps — считаем только чтения тика
	tracer.failAt = 1    // первое чтение лесенки в тике падает

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &countingMaint{},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if got := tracer.seen.Load(); got != 2 {
		t.Errorf("escalation_steps queries = %d, want 2 (ошибка первого чтения лесенки не должна кешироваться и блокировать повтор для второго инцидента)", got)
	}
	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (первый инцидент упал на ошибке чтения лесенки и корректно не эскалирован в этом тике)", notifier.callCount())
	}
	if call := notifier.last(); call.incidentID != 2 {
		t.Errorf("уведомлён инцидент %d, want 2", call.incidentID)
	}
}
