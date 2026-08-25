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

// newGroupGrouper — РЕАЛЬНЫЙ incidentgroup.Grouper поверх реального
// depsuppress.Suppressor (T4, Step 6 брифа): интеграция host↔группы
// тестируется без фейков резолвера корней. Присваивается в поле
// Evaluator.IncidentGroups структурно (duck-typing groupHook, как
// mockDepChecker в dep_test.go).
func newGroupGrouper(pool *pgxpool.Pool) *incidentgroup.Grouper {
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: depsuppress.NewSuppressor(pool),
	}
}

// seedDepEdge — явное ребро зависимости host(parent) -> host(child) (B5).
func seedDepEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentHostID, childHostID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_host_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parentHostID, childHostID); err != nil {
		t.Fatalf("seed dep edge: %v", err)
	}
}

// seedOpenSilentIncident — уже открытый silent-инцидент хоста, минуя
// оценщик; notified управляет гейтом «информирующего корня» (Р4).
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

// seedOpenDiskIncident — уже открытый (и уведомлённый) disk-инцидент хоста,
// минуя оценщик: кандидат ретро-присоединения (Р7).
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

// readGroupID — group_id инцидента (nil — вне групп).
func readGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM host_incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	return gid
}

// seedGroupChannel — включённый канал проекта с возвратом id (в отличие от
// seedAlertChannel): тесту планировщика нужны конкретные каналы ступеней.
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

// groupStepNotifier — фейковый escalation.StepNotifier (образец
// internal/escalation/scheduler_test.go), копит НОМЕРА отправленных ступеней:
// тесту анти-залпа важно, какая именно ступень ушла на каждом тике, а не
// только их число (fakeNotifier пакета номер ступени не хранит).
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

// TestEvaluatorGroupsMemberSilenced — «host silent → алерт этого хоста молчит
// и в составе» (сценарий 1 брифа), host-часть: disk-инцидент ребёнка под
// ИНФОРМИРУЮЩИМ silent-корнем (notified_open=true) присоединяется к группе,
// а его собственный notifyOpen НЕ зовётся — информирует корень.
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

	// Корень упал: молчит 10 минут, его silent-инцидент открыт и УЖЕ
	// уведомлён (информирующий, Р4).
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

// TestEvaluatorGroupsSilentRootMemberNotifies — немой корень (MAJOR-3):
// root silent с notified_open=false (сам не уведомлял — например, открыт в
// maintenance или на B5-гейте) → член в составе группы, но уведомляет САМ
// (fail-noisy): NotifyStep у фейка зафиксирован.
func TestEvaluatorGroupsSilentRootMemberNotifies(t *testing.T) {
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
	seedOpenSilentIncident(t, pool, pid, root.ID, false) // немой корень

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

// TestOpenUnackedGreatestAfterGroupResolve — анти-залп (BLOCKER-1, механизм):
// член ЗАКРЫТОЙ группы возвращается в OpenUnacked со StartedAt = resolved_at
// группы, а не с исходным started_at трёхчасовой давности — лесенка бывшего
// члена стартует с нуля от момента освобождения.
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

// TestOpenUnackedExcludesOpenGroupMembers — член ОТКРЫТОЙ группы исключён из
// OpenUnacked (Р5: информирование берёт на себя корень); после Resolve
// группы — снова в выборке (досылка step0 штатна).
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

// TestEvaluatorRootOpenRetroAttach — ретро (Р7) через evaluator: disk-алерт
// ребёнка опередил смерть корня; открытие silent-корня через evalSilent
// присоединяет его задним числом (group_id появился).
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
	// Корень замолкает ПОСЛЕ того, как disk-инцидент ребёнка уже открыт.
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

// TestGroupedMemberClosesSilently — «член закрылся в группе — close-
// уведомления нет» (§8 спеки, фикс ревью плана M-1): open-уведомление члена
// информирующей группы было подавлено → лог incident_escalations пуст →
// notifyClose (RecoveryChannels) не находит адресатов, NotifyRecovery не
// зовётся. Канал проекта заведён нарочно: молчание обязано объясняться
// пустым логом эскалации, а не отсутствием каналов.
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

	// Диск ребёнка восстановился — член закрывается молча внутри группы.
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

// TestGroupedMemberInMaintenance — «maintenance поверх группы» (§8 спеки,
// фикс ревью плана M-2): член открывается при inMaint=true под информирующим
// корнем → group_id проставлен (состав собран), NotifyStep НЕ вызван,
// notified_open=false — maintenance-гейт и D3-гейт не конфликтуют.
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

// TestSchedulerNoBurstAfterGroupResolve — анти-залп ЧЕРЕЗ ПЛАНИРОВЩИК
// (BLOCKER-1 ревью дизайна, фикс ревью плана M-3; спека §8: «после Resolve
// группы step1 НЕ летит на следующем тике»): реальный escalation.Scheduler
// поверх реального host.IncidentService. Член группы открыт 3 часа назад;
// после Resolve группы Tick №1 шлёт ТОЛЬКО step0, Tick №2 сразу следом
// step1 НЕ шлёт — elapsed считается от resolved_at группы (≈0), а не от
// started_at (3ч, при котором step1 с delay=10м был бы дью немедленно).
// Ловит регрессию elapsed в scheduler.go (счёт от другого таймстемпа) —
// прокси-тест GREATEST выше её не поймал бы.
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
	// Корень восстановился: его инцидент закрыт (не мешает выборке
	// планировщика), группа распущена — resolved_at = now().
	if ok, err := svc.Resolve(ctx, rootInc, 0); err != nil || !ok {
		t.Fatalf("Resolve root incident: ok=%v err=%v", ok, err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
	}

	c1 := seedGroupChannel(t, pool, pid)
	c2 := seedGroupChannel(t, pool, pid)
	policy := escalation.NewPolicyStore(pool)
	// Host-инциденты всегда severity='critical' (DEFAULT 0077).
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

	sched.Tick(ctx) // Tick №1: досылка step0 бывшему члену
	if got := stepNotifier.sentSteps(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("после Tick №1 ушли ступени %v, want ровно [0]", got)
	}

	sched.Tick(ctx) // Tick №2 сразу следом: step1 (delay 10м) НЕ дью
	if got := stepNotifier.sentSteps(); len(got) != 1 {
		t.Fatalf("после Tick №2 ушли ступени %v, want по-прежнему [0]: step1 полетел очередью — elapsed считается от started_at, а не от resolved_at группы (залп BLOCKER-1)", got)
	}
}

// TestEvaluatorCascadeIntermediateRetroAttachesGrandchild — «каскад сверху
// вниз» (R3, W25): A — корень, уже упавший; B — dep-child A, падает
// ПОЗЖЕ; C — dep-child B, чей disk-инцидент открылся ДО падения B. До этой
// правки groupRootOpened выходила при attachedAsMember=true (B
// присоединился членом группы A) и ретро-перебор не запускался вовсе — C
// оставался вне группы навсегда: DownRoot(C) в момент открытия disk-
// инцидента C ещё не находил down-предка (B тогда был жив), а
// OnRootOpened(A) отработал ещё раньше, до падения B, и заново не звался.
// Падение B обязано перезапустить ретро-перебор, но по ФАКТИЧЕСКОМУ корню
// каскада (A), не по узлу B.
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

	// C: disk-инцидент открыт ДО того, как B ушёл в silent.
	memberInc := seedOpenDiskIncident(t, pool, pid, c.ID)

	// Прод держит ОДИН Suppressor на процесс и для Grouper.Roots, и для
	// Evaluator.Dep (см. main.go, depSuppressor) — общий 5с-кеш снимка;
	// тест обязан воспроизвести то же разделение, иначе DownRoot внутри
	// groupRootOpened не сможет узнать фактический корень каскада.
	sup := depsuppress.NewSuppressor(pool)
	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Dep = sup
	eval.IncidentGroups = &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: sup,
	}

	// Тик 1: A замолкает, становится корнем собственной группы.
	setHostLastSeen(t, pool, a.ID, time.Now().UTC().Add(-10*time.Minute))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick (A down): %v", err)
	}
	if gid := readGroupID(t, pool, memberInc); gid != nil {
		t.Fatalf("до падения B C уже в группе (gid=%v) — сценарий теста сломан, B ещё жив", *gid)
	}

	// Тик 2: B тоже замолкает (падение ПРОМЕЖУТОЧНОГО узла).
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
