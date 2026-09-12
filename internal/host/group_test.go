package host_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func newGroupGrouper(pool *pgxpool.Pool) *incidentgroup.Grouper {
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: depsuppress.NewSuppressor(pool),
	}
}

func seedDepEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentHostID, childHostID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_host_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parentHostID, childHostID); err != nil {
		t.Fatalf("seed dep edge: %v", err)
	}
}

func seedMonitorHostEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentMonitorID, childHostID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parentMonitorID, childHostID); err != nil {
		t.Fatalf("seed monitor->host dep edge: %v", err)
	}
}

func seedGroupMonitor(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,$2,'http',60) RETURNING id`, projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	return id
}

func seedOpenMonitorIncident(t *testing.T, pool *pgxpool.Pool, monitorID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO incidents (monitor_id, cause, notified_open)
		VALUES ($1,'boom',true) RETURNING id`, monitorID).Scan(&id); err != nil {
		t.Fatalf("seed monitor incident: %v", err)
	}
	return id
}

func seedOpenSilentIncident(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, notified bool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',$3) RETURNING id`,
		projectID, hostID, notified).Scan(&id); err != nil {
		t.Fatalf("seed silent incident: %v", err)
	}
	return id
}

func seedOpenDiskIncident(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'disk','open',0.95,0.95,'',true) RETURNING id`,
		projectID, hostID).Scan(&id); err != nil {
		t.Fatalf("seed disk incident: %v", err)
	}
	return id
}

func readGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM host_incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	return gid
}

func seedGroupChannel(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO alert_channels (project_id, kind, enabled, target) VALUES ($1,'email',true,'a@b.c') RETURNING id",
		projectID).Scan(&id); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	return id
}

type groupStepNotifier struct {
	mu    sync.Mutex
	steps []int
}

func (n *groupStepNotifier) NotifyStep(_ context.Context, _ int64, channelIDs []int64, step int) ([]int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.steps = append(n.steps, step)
	return channelIDs, nil
}

func (n *groupStepNotifier) sentSteps() []int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]int(nil), n.steps...)
}

func TestEvaluatorGroupsMemberSilenced(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	root := seedEvalHost(t, pool, pid, "gw-01")
	child := seedEvalHost(t, pool, pid, "web-01")
	seedDepEdge(t, pool, pid, root.ID, child.ID)

	setHostLastSeen(t, pool, root.ID, time.Now().UTC().Add(-10*time.Minute))
	rootInc := seedOpenSilentIncident(t, pool, pid, root.ID, true)

	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", child.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = newGroupGrouper(pool)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, child.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident ребёнка должен открыться — группа глушит уведомление, не сам инцидент")
	}
	gid := readGroupID(t, pool, in.ID)
	if gid == nil {
		t.Fatal("group_id IS NULL — член не присоединён к группе корня")
	}
	var gotRootInc int64
	if err := pool.QueryRow(ctx,
		`SELECT root_incident_id FROM incident_groups WHERE id = $1`, *gid).Scan(&gotRootInc); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	if gotRootInc != rootInc {
		t.Errorf("группа якорится на инцидент %d, want silent-корень %d", gotRootInc, rootInc)
	}
	if notifier.openedCount() != 0 {
		t.Errorf("opened notifications = %d, want 0 (информирует корень, NotifyStep члена подавлен)", notifier.openedCount())
	}
	if in.NotifiedOpen {
		t.Error("notified_open = true у члена информирующей группы, want false")
	}
}

func TestEvaluatorGroupsSilentRootMemberNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	root := seedEvalHost(t, pool, pid, "gw-01")
	child := seedEvalHost(t, pool, pid, "web-01")
	seedDepEdge(t, pool, pid, root.ID, child.ID)

	setHostLastSeen(t, pool, root.ID, time.Now().UTC().Add(-10*time.Minute))
	seedOpenSilentIncident(t, pool, pid, root.ID, false)

	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", child.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = newGroupGrouper(pool)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, child.ID, "disk")
	if err != nil || !open {
		t.Fatalf("OpenFor: open=%v err=%v", open, err)
	}
	if readGroupID(t, pool, in.ID) == nil {
		t.Error("group_id IS NULL — под немым корнем член всё равно должен войти в состав группы")
	}
	if notifier.openedCount() != 1 {
		t.Errorf("opened notifications = %d, want 1 (немой корень не информирует — член уведомляет сам)", notifier.openedCount())
	}
}

func TestOpenUnackedGreatestAfterGroupResolve(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	rootH := seedEvalHost(t, pool, pid, "gw-01")
	memberH := seedEvalHost(t, pool, pid, "web-01")

	svc := host.NewIncidentService(pool)
	member, _, err := svc.Open(ctx, pid, memberH.ID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open member: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE host_incidents SET started_at = now() - interval '3 hours' WHERE id = $1`, member.ID); err != nil {
		t.Fatalf("age member: %v", err)
	}

	rootInc := seedOpenSilentIncident(t, pool, pid, rootH.ID, true)
	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "host", rootInc, "host", rootH.ID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "host", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	var got *escalation.PendingIncident
	for i := range list {
		if list[i].ID == member.ID {
			got = &list[i]
		}
	}
	if got == nil {
		t.Fatalf("член закрытой группы отсутствует в OpenUnacked: %+v", list)
	}
	if age := time.Since(got.StartedAt); age >= time.Minute {
		t.Errorf("StartedAt отстаёт на %v — база отсчёта лесенки не GREATEST(started_at, resolved_at): step1 с delay>0 на следующем тике стал бы дью и дал залп", age)
	}
}

func TestOpenUnackedExcludesOpenGroupMembers(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	rootH := seedEvalHost(t, pool, pid, "gw-01")
	memberH := seedEvalHost(t, pool, pid, "web-01")

	svc := host.NewIncidentService(pool)
	member, _, err := svc.Open(ctx, pid, memberH.ID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open member: %v", err)
	}
	rootInc := seedOpenSilentIncident(t, pool, pid, rootH.ID, true)
	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "host", rootInc, "host", rootH.ID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "host", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	contains := func(list []escalation.PendingIncident, id int64) bool {
		for _, p := range list {
			if p.ID == id {
				return true
			}
		}
		return false
	}

	list, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked (группа открыта): %v", err)
	}
	if contains(list, member.ID) {
		t.Error("член ОТКРЫТОЙ группы попал в OpenUnacked — планировщик эскалировал бы его в обход корня")
	}

	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
	}
	list, err = svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked (группа закрыта): %v", err)
	}
	if !contains(list, member.ID) {
		t.Error("член закрытой группы не вернулся в OpenUnacked — досылка step0 после распада группы потеряна")
	}
}

func TestEvaluatorRootOpenRetroAttach(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	root := seedEvalHost(t, pool, pid, "gw-01")
	child := seedEvalHost(t, pool, pid, "web-01")
	seedDepEdge(t, pool, pid, root.ID, child.ID)

	memberInc := seedOpenDiskIncident(t, pool, pid, child.ID)
	setHostLastSeen(t, pool, root.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = newGroupGrouper(pool)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	rootIn, open, err := incidents.OpenFor(ctx, root.ID, "silent")
	if err != nil || !open {
		t.Fatalf("OpenFor root silent: open=%v err=%v", open, err)
	}
	gid := readGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("group_id IS NULL — открытие корня не ретро-присоединило уже открытый disk-инцидент ребёнка")
	}
	var gotRootInc int64
	if err := pool.QueryRow(ctx,
		`SELECT root_incident_id FROM incident_groups WHERE id = $1`, *gid).Scan(&gotRootInc); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	if gotRootInc != rootIn.ID {
		t.Errorf("группа якорится на инцидент %d, want свежеоткрытый silent-корень %d", gotRootInc, rootIn.ID)
	}
}

func TestGroupedMemberClosesSilently(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	root := seedEvalHost(t, pool, pid, "gw-01")
	child := seedEvalHost(t, pool, pid, "web-01")
	seedDepEdge(t, pool, pid, root.ID, child.ID)
	setHostLastSeen(t, pool, root.ID, time.Now().UTC().Add(-10*time.Minute))
	seedOpenSilentIncident(t, pool, pid, root.ID, true)

	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", child.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = newGroupGrouper(pool)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}
	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, child.ID, "disk")
	if err != nil || !open {
		t.Fatalf("OpenFor: open=%v err=%v", open, err)
	}
	if readGroupID(t, pool, in.ID) == nil {
		t.Fatal("setup: член не присоединён к группе")
	}
	if notifier.openedCount() != 0 {
		t.Fatalf("setup: opened notifications = %d, want 0 (подавлено информирующим корнем)", notifier.openedCount())
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", child.Name, map[string]string{"mountpoint": "/"}, 0.50, time.Minute)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}

	_, open, err = incidents.OpenFor(ctx, child.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor after resolve: %v", err)
	}
	if open {
		t.Error("disk incident члена должен закрыться при 0.50 < порога")
	}
	if notifier.resolvedCount() != 0 {
		t.Errorf("resolved notifications = %d, want 0 (RecoveryChannels пуст → тишина)", notifier.resolvedCount())
	}
}

func TestGroupedMemberInMaintenance(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	root := seedEvalHost(t, pool, pid, "gw-01")
	child := seedEvalHost(t, pool, pid, "web-01")
	seedDepEdge(t, pool, pid, root.ID, child.ID)
	setHostLastSeen(t, pool, root.ID, time.Now().UTC().Add(-10*time.Minute))
	seedOpenSilentIncident(t, pool, pid, root.ID, true)

	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", child.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.IncidentGroups = newGroupGrouper(pool)
	eval.Maint = mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil })

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, child.ID, "disk")
	if err != nil || !open {
		t.Fatalf("OpenFor: open=%v err=%v", open, err)
	}
	if readGroupID(t, pool, in.ID) == nil {
		t.Error("group_id IS NULL — состав группы должен собираться и в maintenance")
	}
	if notifier.openedCount() != 0 {
		t.Errorf("opened notifications = %d, want 0 (maintenance + группа)", notifier.openedCount())
	}
	if in.NotifiedOpen {
		t.Error("notified_open = true, want false")
	}
}

func TestSchedulerNoBurstAfterGroupResolve(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	rootH := seedEvalHost(t, pool, pid, "gw-01")
	memberH := seedEvalHost(t, pool, pid, "web-01")

	svc := host.NewIncidentService(pool)
	member, _, err := svc.Open(ctx, pid, memberH.ID, "disk", 0.95, "", false)
	if err != nil {
		t.Fatalf("Open member: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE host_incidents SET started_at = now() - interval '3 hours' WHERE id = $1`, member.ID); err != nil {
		t.Fatalf("age member: %v", err)
	}

	rootInc := seedOpenSilentIncident(t, pool, pid, rootH.ID, true)
	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "host", rootInc, "host", rootH.ID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, pid, "host", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	if ok, err := svc.Resolve(ctx, rootInc, 0); err != nil || !ok {
		t.Fatalf("Resolve root incident: ok=%v err=%v", ok, err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
	}

	c1 := seedGroupChannel(t, pool, pid)
	c2 := seedGroupChannel(t, pool, pid)
	policy := escalation.NewPolicyStore(pool)
	if err := policy.SetLadder(ctx, pid, escalation.SeverityCritical, []escalation.Step{
		{StepNo: 0, DelayMinutes: 0, ChannelIDs: []int64{c1}},
		{StepNo: 1, DelayMinutes: 10, ChannelIDs: []int64{c2}},
	}); err != nil {
		t.Fatalf("SetLadder: %v", err)
	}

	stepNotifier := &groupStepNotifier{}
	sched := &escalation.Scheduler{
		Bindings: []escalation.Binding{{Src: svc, Notifier: stepNotifier}},
		Policy:   policy,
		Maint:    mockMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil }),
		Pool:     pool,
		Now:      time.Now,
	}

	sched.Tick(ctx)
	if got := stepNotifier.sentSteps(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("после Tick №1 ушли ступени %v, want ровно [0]", got)
	}

	sched.Tick(ctx)
	if got := stepNotifier.sentSteps(); len(got) != 1 {
		t.Fatalf("после Tick №2 ушли ступени %v, want по-прежнему [0]: step1 полетел очередью — elapsed считается от started_at, а не от resolved_at группы (залп BLOCKER-1)", got)
	}
}

func TestEvaluatorCascadeIntermediateRetroAttachesGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	a := seedEvalHost(t, pool, pid, "gw-01")
	b := seedEvalHost(t, pool, pid, "sw-01")
	c := seedEvalHost(t, pool, pid, "web-01")
	seedDepEdge(t, pool, pid, a.ID, b.ID)
	seedDepEdge(t, pool, pid, b.ID, c.ID)

	memberInc := seedOpenDiskIncident(t, pool, pid, c.ID)

	sup := depsuppress.NewSuppressor(pool)
	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = sup
	eval.IncidentGroups = &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: sup,
	}

	setHostLastSeen(t, pool, a.ID, time.Now().UTC().Add(-10*time.Minute))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (A down): %v", err)
	}
	if gid := readGroupID(t, pool, memberInc); gid != nil {
		t.Fatalf("до падения B C уже в группе (gid=%v) — сценарий теста сломан, B ещё жив", *gid)
	}

	setHostLastSeen(t, pool, b.ID, time.Now().UTC().Add(-10*time.Minute))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (B down): %v", err)
	}

	incidents := host.NewIncidentService(pool)
	rootIn, open, err := incidents.OpenFor(ctx, a.ID, "silent")
	if err != nil || !open {
		t.Fatalf("OpenFor A silent: open=%v err=%v", open, err)
	}

	gid := readGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("group_id IS NULL — падение промежуточного узла B не ретро-присоединило C, открытого до падения B")
	}
	var gotRootInc int64
	if err := pool.QueryRow(ctx,
		`SELECT root_incident_id FROM incident_groups WHERE id = $1`, *gid).Scan(&gotRootInc); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	if gotRootInc != rootIn.ID {
		t.Errorf("группа якорится на инцидент %d, want фактический корень каскада — silent-инцидент A (%d)", gotRootInc, rootIn.ID)
	}
}

func TestEvaluatorCascadeThroughMonitorRootAttachesGrandchild(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	mon := seedGroupMonitor(t, pool, pid, "https-root")
	a := seedEvalHost(t, pool, pid, "gw-01")
	c := seedEvalHost(t, pool, pid, "web-01")
	seedMonitorHostEdge(t, pool, pid, mon, a.ID)
	seedDepEdge(t, pool, pid, a.ID, c.ID)

	monInc := seedOpenMonitorIncident(t, pool, mon)

	memberInc := seedOpenDiskIncident(t, pool, pid, c.ID)

	sup := depsuppress.NewSuppressor(pool)
	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = sup
	eval.IncidentGroups = &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: sup,
	}

	setHostLastSeen(t, pool, a.ID, time.Now().UTC().Add(-10*time.Minute))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (A down): %v", err)
	}

	gid := readGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("group_id IS NULL — падение A под монитором-корнем не ретро-присоединило C")
	}
	var rootSource, rootNodeKind string
	var rootIncID, rootNodeID int64
	if err := pool.QueryRow(ctx,
		`SELECT root_source, root_incident_id, root_node_kind, root_node_id FROM incident_groups WHERE id = $1`,
		*gid).Scan(&rootSource, &rootIncID, &rootNodeKind, &rootNodeID); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	if rootSource != "uptime" || rootIncID != monInc || rootNodeKind != "monitor" || rootNodeID != mon {
		t.Errorf("группа якорится на %s/%d (node %s/%d), want фактический корень каскада — монитор %d (инцидент %d)",
			rootSource, rootIncID, rootNodeKind, rootNodeID, mon, monInc)
	}

	incidents := host.NewIncidentService(pool)
	aIn, open, err := incidents.OpenFor(ctx, a.ID, "silent")
	if err != nil || !open {
		t.Fatalf("OpenFor A silent: open=%v err=%v", open, err)
	}
	aGid := readGroupID(t, pool, aIn.ID)
	if aGid == nil || *aGid != *gid {
		t.Errorf("A.group_id = %v, want %d (та же группа монитора, что и у C)", aGid, *gid)
	}
}
