package escalation_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Не вызывать из тестов с t.Parallel(): slog.Default глобален для процесса.
func captureInfoLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type fakeSource struct {
	mu         sync.Mutex
	name       string
	incs       []*fakeIncident
	suppressed map[int64]bool
	// Тесты с ожиданием мгновенного elapsed после освобождения подставляют тот
	// же clock, что и Scheduler.Now — иначе StartedAt даст отрицательный elapsed.
	clock              func() time.Time
	openSuppressedErr  error
	clearSuppressedErr error
}

type fakeIncident struct {
	inc   escalation.PendingIncident
	acked bool
}

func newFakeSource(name string) *fakeSource {
	return &fakeSource{name: name}
}

func (s *fakeSource) add(inc escalation.PendingIncident) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.incs = append(s.incs, &fakeIncident{inc: inc})
}

func (s *fakeSource) ack(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.incs {
		if i.inc.ID == id {
			i.acked = true
		}
	}
}

func (s *fakeSource) level(id int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.incs {
		if i.inc.ID == id {
			return i.inc.EscalationLevel
		}
	}
	return -1
}

func (s *fakeSource) Name() string { return s.name }

func (s *fakeSource) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []escalation.PendingIncident
	for _, i := range s.incs {
		if !i.acked && !s.suppressed[i.inc.ID] {
			out = append(out, i.inc)
		}
	}
	return out, nil
}

func (s *fakeSource) markSuppressed(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suppressed == nil {
		s.suppressed = make(map[int64]bool)
	}
	s.suppressed[id] = true
}

func (s *fakeSource) OpenSuppressed(ctx context.Context) ([]escalation.PendingIncident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []escalation.PendingIncident
	for _, i := range s.incs {
		if s.suppressed[i.inc.ID] {
			out = append(out, i.inc)
		}
	}
	if s.openSuppressedErr != nil {
		// Ошибка и частичный список вместе: pgx.Query может отдать частично
		// прочитанные строки до ошибки — тест ловит потерю guard'а `if err != nil { return }`.
		return out, s.openSuppressedErr
	}
	return out, nil
}

func (s *fakeSource) ClearSuppressed(ctx context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clearSuppressedErr != nil {
		return s.clearSuppressedErr
	}
	delete(s.suppressed, id)
	clock := s.clock
	if clock == nil {
		clock = time.Now
	}
	for _, i := range s.incs {
		if i.inc.ID == id {
			i.inc.StartedAt = clock()
		}
	}
	return nil
}

func (s *fakeSource) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.incs {
		if i.inc.ID == id {
			if i.inc.EscalationLevel != from {
				return false, nil
			}
			i.inc.EscalationLevel = from + 1
			return true, nil
		}
	}
	return false, nil
}

// Реализует только escalation.Source, без SuppressedSource — как настоящие
// metric/profile/slo/trace, у которых графа зависимостей нет архитектурно.
type bareSource struct {
	name string
	inc  escalation.PendingIncident
}

func (s *bareSource) Name() string { return s.name }

func (s *bareSource) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	return []escalation.PendingIncident{s.inc}, nil
}

func (s *bareSource) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	return true, nil
}

type fakeMaint struct {
	inMaint bool
	err     error
}

func (m *fakeMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m.inMaint, m.err
}

type fakeDep struct {
	mu         sync.Mutex
	hasParent  bool
	parentDown bool
	checkErr   error
	markErr    error
	markCalls  []fakeMarkCall
	checkedSrc []string
}

type fakeMarkCall struct {
	source     string
	incidentID int64
}

// Ветвится по имени как настоящий depsuppress.Suppressor: только host/uptime
// резолвятся, остальное — ошибка, а не молчаливое hasParent=false.
func (d *fakeDep) CheckIncident(ctx context.Context, source string, incidentID int64) (bool, bool, error) {
	d.mu.Lock()
	d.checkedSrc = append(d.checkedSrc, source)
	d.mu.Unlock()
	if d.checkErr != nil {
		return false, false, d.checkErr
	}
	switch source {
	case "host", "uptime":
		return d.hasParent, d.parentDown, nil
	default:
		return false, false, errors.New("fakeDep: unknown source: " + source)
	}
}

func (d *fakeDep) checkedSources() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.checkedSrc...)
}

func (d *fakeDep) MarkSuppressed(ctx context.Context, source string, incidentID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.markCalls = append(d.markCalls, fakeMarkCall{source: source, incidentID: incidentID})
	return d.markErr
}

func (d *fakeDep) markCallCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.markCalls)
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []fakeNotifyCall
}

type fakeNotifyCall struct {
	incidentID int64
	channelIDs []int64
	step       int
}

func (n *fakeNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fakeNotifyCall{incidentID: incidentID, channelIDs: channelIDs, step: step})
	return channelIDs, nil
}

func (n *fakeNotifier) callCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.calls)
}

func (n *fakeNotifier) last() fakeNotifyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[len(n.calls)-1]
}

func (n *fakeNotifier) allCalls() []fakeNotifyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]fakeNotifyCall(nil), n.calls...)
}

func setLadder(t *testing.T, policy *escalation.PolicyStore, projectID int64, severity string, steps []escalation.Step) {
	t.Helper()
	if err := policy.SetLadder(context.Background(), projectID, severity, steps); err != nil {
		t.Fatalf("SetLadder: %v", err)
	}
}

func TestSchedulerTickEscalatesWhenStepDelayDue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
		{StepNo: 1, DelayMinutes: 5, ChannelIDs: []int64{c2}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("metric")
	const incidentID = int64(1)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-6 * time.Minute),
		Severity: escalation.SeverityWarning, EscalationLevel: 1,
	})
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1", notifier.callCount())
	}
	call := notifier.last()
	if call.incidentID != incidentID || call.step != 1 || len(call.channelIDs) != 1 || call.channelIDs[0] != c2 {
		t.Fatalf("NotifyStep call = %+v, want incident=%d step=1 channels=[%d]", call, incidentID, c2)
	}
	if got := src.level(incidentID); got != 2 {
		t.Fatalf("EscalationLevel = %d, want 2", got)
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND channel_id=$2 AND step=1",
		incidentID, c2).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 1 {
		t.Fatalf("incident_escalations rows = %d, want 1", count)
	}
}

func TestSchedulerTickSkipsWhenStepDelayNotDue(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 5, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("metric")
	const incidentID = int64(2)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-1 * time.Minute),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (delay ещё не настал)", notifier.callCount())
	}
	if got := src.level(incidentID); got != 0 {
		t.Fatalf("EscalationLevel = %d, want 0 (не тронут)", got)
	}
}

func TestSchedulerTickSkipsInMaintenance(t *testing.T) {
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
	const incidentID = int64(3)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: true},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (окно обслуживания)", notifier.callCount())
	}
	if got := src.level(incidentID); got != 0 {
		t.Fatalf("EscalationLevel = %d, want 0 (не тронут)", got)
	}
}

func TestSchedulerTickEscalatesAfterMaintenanceEnds(t *testing.T) {
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
	const incidentID = int64(4)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (окно закончилось)", notifier.callCount())
	}
	if got := src.level(incidentID); got != 1 {
		t.Fatalf("EscalationLevel = %d, want 1", got)
	}
}

func TestSchedulerTickIgnoresAckedIncidents(t *testing.T) {
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
	const incidentID = int64(5)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.ack(incidentID)
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (инцидент подтверждён, OpenUnacked его не отдаёт)", notifier.callCount())
	}
	if got := src.level(incidentID); got != 0 {
		t.Fatalf("EscalationLevel = %d, want 0 (не тронут)", got)
	}
}

func TestSchedulerTickIdempotentOnRepeatedTick(t *testing.T) {
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
	const incidentID = int64(6)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)
	if notifier.callCount() != 1 {
		t.Fatalf("после первого Tick calls = %d, want 1", notifier.callCount())
	}
	sched.Tick(ctx)
	if notifier.callCount() != 1 {
		t.Fatalf("после второго Tick calls = %d, want 1 (лесенка исчерпана, не дублирует)", notifier.callCount())
	}

	var count int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1",
		incidentID).Scan(&count); err != nil {
		t.Fatalf("select escalation log: %v", err)
	}
	if count != 1 {
		t.Fatalf("incident_escalations rows = %d, want 1 (второй Tick не задублировал лог)", count)
	}
}

func TestSchedulerRunStopsOnContextCancel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("metric")
	const incidentID = int64(7)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Pool:     pool,
		Interval: 2 * time.Millisecond,
		Now:      func() time.Time { return now },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for notifier.callCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("Run не сделал ни одного тика — цикл не работает")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Scheduler.Run did not return after ctx cancel")
	}
}

func TestPurgeOldEscalations(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	if err := escalation.LogStep(ctx, pool, "metric", 100, c1, 0); err != nil {
		t.Fatalf("LogStep old: %v", err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE incident_escalations SET sent_at = now() - interval '100 days' WHERE incident_id=100"); err != nil {
		t.Fatalf("age old row: %v", err)
	}
	if err := escalation.LogStep(ctx, pool, "metric", 101, c1, 0); err != nil {
		t.Fatalf("LogStep fresh: %v", err)
	}

	n, err := escalation.PurgeOldEscalations(ctx, pool, 90*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeOldEscalations: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged = %d, want 1", n)
	}

	var oldCount, freshCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incident_escalations WHERE incident_id=100").Scan(&oldCount); err != nil {
		t.Fatalf("count old: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM incident_escalations WHERE incident_id=101").Scan(&freshCount); err != nil {
		t.Fatalf("count fresh: %v", err)
	}
	if oldCount != 0 {
		t.Errorf("old row survived purge: count=%d, want 0", oldCount)
	}
	if freshCount != 1 {
		t.Errorf("fresh row purged: count=%d, want 1", freshCount)
	}
}

func TestTickSuppressesWhenParentDown(t *testing.T) {
	for _, level := range []int{0, 2} {
		t.Run(map[int]string{0: "level0", 2: "level2"}[level], func(t *testing.T) {
			pool := testenv.MigratedPG(t)
			ctx := context.Background()
			pid := newProject(t, pool)
			c1 := newChannel(t, pool, pid, true)

			policy := escalation.NewPolicyStore(pool)
			setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
				{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
				{StepNo: 1, DelayMinutes: 0, ChannelIDs: []int64{c1}},
				{StepNo: 2, DelayMinutes: 0, ChannelIDs: []int64{c1}},
			})

			now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
			src := newFakeSource("host")
			incidentID := int64(1000 + level)
			src.add(escalation.PendingIncident{
				ID: incidentID, ProjectID: pid, StartedAt: now,
				Severity: escalation.SeverityWarning, EscalationLevel: level,
			})
			notifier := &fakeNotifier{}
			dep := &fakeDep{hasParent: true, parentDown: true}

			sched := &escalation.Scheduler{
				Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
				Policy:   policy,
				Maint:    &fakeMaint{inMaint: false},
				Dep:      dep,
				Pool:     pool,
				Now:      func() time.Time { return now },
			}
			sched.Tick(ctx)

			if notifier.callCount() != 0 {
				t.Fatalf("NotifyStep calls = %d, want 0 (родитель упал — подавлен)", notifier.callCount())
			}
			if dep.markCallCount() != 1 {
				t.Fatalf("MarkSuppressed calls = %d, want 1", dep.markCallCount())
			}
			call := dep.markCalls[0]
			if call.source != "host" || call.incidentID != incidentID {
				t.Fatalf("MarkSuppressed call = %+v, want source=host incident=%d", call, incidentID)
			}
		})
	}
}

func TestTickCheckErrorEscalatesFailSafe(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("host")
	const incidentID = int64(4000)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: true, checkErr: errors.New("dep db down")}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (ошибка dep-проверки → fail-safe эскалация)", notifier.callCount())
	}
	if dep.markCallCount() != 0 {
		t.Fatalf("MarkSuppressed calls = %d, want 0 (ошибка не подавляет)", dep.markCallCount())
	}
	if got := src.level(incidentID); got != 1 {
		t.Fatalf("EscalationLevel = %d, want 1 (эскалация прошла)", got)
	}
}

func TestTickMarkSuppressedErrorStillSuppressesThisTick(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("host")
	const incidentID = int64(5000)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: true, markErr: errors.New("mark db down")}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (родитель упал — ступень не шлём даже при ошибке пометки)", notifier.callCount())
	}
	if dep.markCallCount() != 1 {
		t.Fatalf("MarkSuppressed calls = %d, want 1", dep.markCallCount())
	}

	sched.Tick(ctx)
	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (повтор тика по-прежнему подавляет)", notifier.callCount())
	}
	if dep.markCallCount() != 2 {
		t.Fatalf("MarkSuppressed calls = %d, want 2 (следующий тик повторяет пометку)", dep.markCallCount())
	}
}

func TestTickHoldsStep0DuringGrace(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("host")
	const incidentID = int64(2000)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-100 * time.Second),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: false}

	sched := &escalation.Scheduler{
		Bindings:    []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:      policy,
		Maint:       &fakeMaint{inMaint: false},
		Dep:         dep,
		SettleGrace: 300 * time.Second,
		Pool:        pool,
		Now:         func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (в грейсе)", notifier.callCount())
	}
	if dep.markCallCount() != 0 {
		t.Fatalf("MarkSuppressed calls = %d, want 0 (родитель жив)", dep.markCallCount())
	}
}

func TestTickSendsAfterGrace(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	src := newFakeSource("host")
	const incidentID = int64(3000)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-301 * time.Second),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: false}

	sched := &escalation.Scheduler{
		Bindings:    []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:      policy,
		Maint:       &fakeMaint{inMaint: false},
		Dep:         dep,
		SettleGrace: 300 * time.Second,
		Pool:        pool,
		Now:         func() time.Time { return now },
	}
	sched.Tick(ctx)

	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (грейс истёк)", notifier.callCount())
	}
	call := notifier.last()
	if call.incidentID != incidentID || call.step != 0 {
		t.Fatalf("NotifyStep call = %+v, want incident=%d step=0", call, incidentID)
	}
	if dep.markCallCount() != 0 {
		t.Fatalf("MarkSuppressed calls = %d, want 0 (родитель жив)", dep.markCallCount())
	}
}

type blockingSource struct {
	calls atomic.Int64
}

func (b *blockingSource) Name() string { return "blocking" }

func (b *blockingSource) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	b.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingSource) BumpEscalation(context.Context, int64, int) (bool, error) {
	return false, nil
}

func TestSchedulerPublishesTickLiveness(t *testing.T) {
	sched := &escalation.Scheduler{Interval: time.Hour, Now: time.Now}
	if got := sched.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	before := time.Now().Unix()
	sched.Tick(context.Background())

	if got := sched.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := sched.LastTickSeconds(); got <= 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

func TestSchedulerTickBudgetAbortsHungTick(t *testing.T) {
	src := &blockingSource{}
	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: &fakeNotifier{}}},
		Interval: time.Second,
		Now:      time.Now,
	}

	done := make(chan struct{})
	go func() {
		sched.Tick(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Tick не завершился: повисший источник блокирует планировщик")
	}

	if src.calls.Load() == 0 {
		t.Error("планировщик не звал OpenUnacked вовсе — тест не проверяет то, что должен")
	}
	if got := sched.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по дедлайну тика, want 0", got)
	}
	if got := sched.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
}

// OpenUnacked сигналит о своём приходе, потом ждёт партнёра — обе реплики
// читают escalation_level=0 до входа в ClaimStepChannels.
type barrierSource struct {
	*fakeSource
	gate *sync.WaitGroup
}

func (b barrierSource) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	b.gate.Done()
	b.gate.Wait()
	return b.fakeSource.OpenUnacked(ctx)
}

func TestSchedulerTwoReplicasDeliverStepOnce(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)
	c2 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1, c2}},
		{StepNo: 1, DelayMinutes: 15, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 10; i++ {
		incidentID := int64(30000 + i)
		src := newFakeSource("metric")
		src.add(escalation.PendingIncident{
			ID: incidentID, ProjectID: pid, StartedAt: now,
			Severity: escalation.SeverityWarning, EscalationLevel: 0,
		})

		var gate sync.WaitGroup
		gate.Add(2)
		bsrc := barrierSource{fakeSource: src, gate: &gate}

		notifier1 := &fakeNotifier{}
		notifier2 := &fakeNotifier{}
		sched1 := &escalation.Scheduler{
			Bindings: []escalation.Binding{{Src: bsrc, Notifier: notifier1}},
			Policy:   policy,
			Maint:    &fakeMaint{inMaint: false},
			Pool:     pool,
			Now:      func() time.Time { return now },
		}
		sched2 := &escalation.Scheduler{
			Bindings: []escalation.Binding{{Src: bsrc, Notifier: notifier2}},
			Policy:   policy,
			Maint:    &fakeMaint{inMaint: false},
			Pool:     pool,
			Now:      func() time.Time { return now },
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); sched1.Tick(ctx) }()
		go func() { defer wg.Done(); sched2.Tick(ctx) }()
		wg.Wait()

		total := notifier1.callCount() + notifier2.callCount()
		if total != 1 {
			t.Fatalf("инцидент #%d (id=%d): сумма вызовов NotifyStep двух реплик = %d, want 1",
				i, incidentID, total)
		}
		if got := src.level(incidentID); got != 1 {
			t.Fatalf("инцидент #%d (id=%d): EscalationLevel = %d, want 1", i, incidentID, got)
		}
		var count int
		if err := pool.QueryRow(ctx,
			"SELECT count(*) FROM incident_escalations WHERE incident_source='metric' AND incident_id=$1 AND step=0",
			incidentID).Scan(&count); err != nil {
			t.Fatalf("инцидент #%d: select escalation log: %v", i, err)
		}
		if count != 2 {
			t.Fatalf("инцидент #%d (id=%d): incident_escalations rows for step0 = %d, want 2", i, incidentID, count)
		}
	}
}

func TestSchedulerReleasesSuppressedIncidentWhenParentRecovers(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := newFakeSource("host")
	const incidentID = int64(40001)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.clock = clock
	src.markSuppressed(incidentID)

	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: false}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	sched.Tick(ctx)

	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (снятый инцидент должен получить ступень 0 в этом же тике)", notifier.callCount())
	}
	call := notifier.last()
	if call.incidentID != incidentID || call.step != 0 {
		t.Fatalf("NotifyStep call = %+v, want incident=%d step=0", call, incidentID)
	}
	if got := src.level(incidentID); got != 1 {
		t.Fatalf("EscalationLevel = %d, want 1", got)
	}
}

func TestSchedulerKeepsSuppressedWhileParentDown(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := newFakeSource("host")
	const incidentID = int64(40002)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.clock = clock
	src.markSuppressed(incidentID)

	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: true}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (родитель ещё лежит — подавление держится)", notifier.callCount())
	}
	if got := src.level(incidentID); got != 0 {
		t.Fatalf("EscalationLevel = %d, want 0 (не снят)", got)
	}
	got, err := src.OpenSuppressed(ctx)
	if err != nil {
		t.Fatalf("OpenSuppressed: %v", err)
	}
	if len(got) != 1 || got[0].ID != incidentID {
		t.Fatalf("OpenSuppressed = %+v, want [инцидент %d] (флаг подавления не снят)", got, incidentID)
	}
}

func TestSchedulerReleasesWhenDependencyRemoved(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := newFakeSource("host")
	const incidentID = int64(40003)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.clock = clock
	src.markSuppressed(incidentID)

	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: false, parentDown: false}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	sched.Tick(ctx)

	if notifier.callCount() != 1 {
		t.Fatalf("NotifyStep calls = %d, want 1 (зависимость удалена — подавлять больше нечем)", notifier.callCount())
	}
	if got := src.level(incidentID); got != 1 {
		t.Fatalf("EscalationLevel = %d, want 1", got)
	}
}

func TestSchedulerReleaseSuppressedOpenSuppressedError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := newFakeSource("host")
	const incidentID = int64(40004)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.clock = clock
	src.markSuppressed(incidentID)
	src.openSuppressedErr = errors.New("open suppressed: db down")

	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: false}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (OpenSuppressed упал — снятие подавления не произошло)", notifier.callCount())
	}
	src.openSuppressedErr = nil
	got, err := src.OpenSuppressed(ctx)
	if err != nil {
		t.Fatalf("OpenSuppressed: %v", err)
	}
	if len(got) != 1 || got[0].ID != incidentID {
		t.Fatalf("OpenSuppressed = %+v, want [инцидент %d] (флаг подавления не тронут)", got, incidentID)
	}
}

func TestSchedulerReleaseSuppressedCheckIncidentError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := newFakeSource("host")
	const incidentID = int64(40005)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.clock = clock
	src.markSuppressed(incidentID)

	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: false, checkErr: errors.New("dep check: db down")}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (CheckIncident упал — снятие подавления не произошло)", notifier.callCount())
	}
	got, err := src.OpenSuppressed(ctx)
	if err != nil {
		t.Fatalf("OpenSuppressed: %v", err)
	}
	if len(got) != 1 || got[0].ID != incidentID {
		t.Fatalf("OpenSuppressed = %+v, want [инцидент %d] (флаг подавления не тронут)", got, incidentID)
	}
}

func TestSchedulerReleaseSuppressedClearSuppressedError(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	src := newFakeSource("host")
	const incidentID = int64(40006)
	src.add(escalation.PendingIncident{
		ID: incidentID, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
		Severity: escalation.SeverityWarning, EscalationLevel: 0,
	})
	src.clock = clock
	src.markSuppressed(incidentID)
	src.clearSuppressedErr = errors.New("clear suppressed: db down")

	notifier := &fakeNotifier{}
	dep := &fakeDep{hasParent: true, parentDown: false}

	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: src, Notifier: notifier}},
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	logs := captureInfoLog(t)
	sched.Tick(ctx)

	if notifier.callCount() != 0 {
		t.Fatalf("NotifyStep calls = %d, want 0 (ClearSuppressed упал — снятие подавления не произошло)", notifier.callCount())
	}
	if !strings.Contains(logs.String(), "clear suppressed failed") {
		t.Fatalf("log = %q, want содержит warn \"clear suppressed failed\"", logs.String())
	}
	if strings.Contains(logs.String(), "dependency recovered") {
		t.Fatalf("log = %q, не должен содержать \"dependency recovered\" — ClearSuppressed провалился", logs.String())
	}
	src.clearSuppressedErr = nil
	got, err := src.OpenSuppressed(ctx)
	if err != nil {
		t.Fatalf("OpenSuppressed: %v", err)
	}
	if len(got) != 1 || got[0].ID != incidentID {
		t.Fatalf("OpenSuppressed = %+v, want [инцидент %d] (флаг подавления не снят)", got, incidentID)
	}
}

// Граф зависимостей есть только у host/uptime (SuppressedSource) — гейт не
// должен звать CheckIncident у источников без него вовсе, иначе шум на тик.
func TestSchedulerOnlyChecksDepsForSuppressedSources(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := newProject(t, pool)
	c1 := newChannel(t, pool, pid, true)

	policy := escalation.NewPolicyStore(pool)
	setLadder(t, policy, pid, escalation.SeverityWarning, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
	})

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	notifier := &fakeNotifier{}

	var bindings []escalation.Binding
	var id int64 = 50000
	for _, name := range []string{"host", "uptime", "metric", "profile", "slo", "trace"} {
		id++
		inc := escalation.PendingIncident{
			ID: id, ProjectID: pid, StartedAt: now.Add(-2 * time.Hour),
			Severity: escalation.SeverityWarning, EscalationLevel: 0,
		}
		switch name {
		case "host", "uptime":
			src := newFakeSource(name)
			src.clock = clock
			src.add(inc)
			bindings = append(bindings, escalation.Binding{Src: src, Notifier: notifier})
		default:
			bindings = append(bindings, escalation.Binding{Src: &bareSource{name: name, inc: inc}, Notifier: notifier})
		}
	}

	dep := &fakeDep{hasParent: false, parentDown: false}
	logs := captureInfoLog(t)

	sched := &escalation.Scheduler{
		Bindings: bindings,
		Policy:   policy,
		Maint:    &fakeMaint{inMaint: false},
		Dep:      dep,
		Pool:     pool,
		Now:      clock,
	}
	sched.Tick(ctx)

	seen := map[string]bool{}
	for _, s := range dep.checkedSources() {
		seen[s] = true
	}
	if len(seen) != 2 || !seen["host"] || !seen["uptime"] {
		t.Fatalf("CheckIncident звался для %v, want ровно {host, uptime}", dep.checkedSources())
	}
	if strings.Contains(logs.String(), "dep check failed") {
		t.Fatalf("log = %q, не должен содержать \"dep check failed\" для источников без графа зависимостей", logs.String())
	}
	if notifier.callCount() != 6 {
		t.Fatalf("NotifyStep calls = %d, want 6 (все шесть инцидентов эскалируются штатно)", notifier.callCount())
	}
}
