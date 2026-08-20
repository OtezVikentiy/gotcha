package escalation_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fakeSource — Source (T4) в памяти: планировщик (T8) работает с ЛЮБЫМ из
// пяти product-сторов через один и тот же интерфейс, поэтому тесты
// планировщика не обязаны тянуть реальные host/metric/trace/profile/slo
// таблицы — только форму интерфейса.
type fakeSource struct {
	mu   sync.Mutex
	name string
	incs []*fakeIncident
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
		if !i.acked {
			out = append(out, i.inc)
		}
	}
	return out, nil
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

// fakeMaint — MaintenanceChecker с фиксированным ответом/ошибкой.
type fakeMaint struct {
	inMaint bool
	err     error
}

func (m *fakeMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m.inMaint, m.err
}

// fakeDep — DepChecker (B5) с настраиваемым ответом CheckIncident и
// фиксацией вызовов MarkSuppressed.
type fakeDep struct {
	mu         sync.Mutex
	hasParent  bool
	parentDown bool
	checkErr   error
	markErr    error
	markCalls  []fakeMarkCall
}

type fakeMarkCall struct {
	source     string
	incidentID int64
}

func (d *fakeDep) CheckIncident(ctx context.Context, source string, incidentID int64) (bool, bool, error) {
	return d.hasParent, d.parentDown, d.checkErr
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

// fakeNotifier — StepNotifier, фиксирующий вызовы и возвращающий переданные
// каналы как «реально заенкенные» (симулирует успешную отправку).
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

// setLadder — хелпер: настраивает лесенку эскалации (project, severity)
// через реальный PolicyStore.
func setLadder(t *testing.T, policy *escalation.PolicyStore, projectID int64, severity string, steps []escalation.Step) {
	t.Helper()
	if err := policy.SetLadder(context.Background(), projectID, severity, steps); err != nil {
		t.Fatalf("SetLadder: %v", err)
	}
}

// TestSchedulerTickEscalatesWhenStepDelayDue дискриминирует «шлёт очередную
// ступень лесенки, когда её задержка от открытия инцидента настала»: инцидент
// уже прошёл ступень 0 (escalation_level=1), лесенка [{0,0,c1},{1,5,c2}]
// (ступень 1 — delay 5 мин), StartedAt=now-6мин → задержка ступени 1 настала.
// Tick шлёт NotifyStep(incident, [c2], step=1), продвигает level 1→2 и
// логирует (step=1, c2) в incident_escalations.
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

// TestSchedulerTickSkipsWhenStepDelayNotDue: задержка ступени ещё не настала
// (elapsed 1мин < delay 5мин) — Tick ничего не шлёт, уровень не двигается.
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

// TestSchedulerTickSkipsInMaintenance: живая проверка окна обслуживания
// (BLOCKER-3) — даже если задержка ступени настала, Tick пропускает инцидент,
// пока проект в окне.
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

// TestSchedulerTickEscalatesAfterMaintenanceEnds: инцидент открылся в окне
// обслуживания (лога ещё нет), окно закончилось (Maint→false) — Tick шлёт
// ступень 0, как только видит окно закрытым.
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

// TestSchedulerTickIgnoresAckedIncidents: подтверждённый инцидент не
// возвращается OpenUnacked (T4) — планировщик его вовсе не видит и не трогает.
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

// TestSchedulerTickIdempotentOnRepeatedTick: два Tick подряд без смены Now —
// второй не дублирует отправку: лесенка из одной ступени исчерпана после
// первого бампа (level=1 >= len(ladder)=1), второй Tick — no-op.
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

// TestSchedulerRunStopsOnContextCancel: Run тикает и корректно завершается по
// отмене ctx, не утекая горутиной — образец notify.OutboxJanitor (janitor_test.go).
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

// TestPurgeOldEscalations: старая строка лога чистится, свежая остаётся.
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

// TestTickSuppressesWhenParentDown (B5/T5, §7.4/MINOR-6): DepChecker сообщает
// parentDown=true — Tick подавляет инцидент навсегда (MarkSuppressed для
// ("host", id)) и НЕ шлёт ступень, причём на ЛЮБОМ уровне эскалации, а не
// только на step0 — гейт душит продолжение эскалации, даже если ребёнок уже
// эскалировал до того, как родитель упал.
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

// TestTickHoldsStep0DuringGrace: родитель жив (parentDown=false), но у
// инцидента есть родитель — ступень 0 придерживается в течение SettleGrace:
// ни NotifyStep, ни MarkSuppressed не вызываются, инцидент просто ждёт
// следующих тиков.
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

// TestTickSendsAfterGrace: тот же сценарий, что и грейс, но elapsed уже
// превысил SettleGrace — ступень 0 уходит штатно, несмотря на живого
// родителя (грейс — не бесконечное молчание).
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
