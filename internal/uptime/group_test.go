package uptime_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// newUptimeGrouper — РЕАЛЬНЫЙ incidentgroup.Grouper поверх реального
// depsuppress.Suppressor (образец internal/host/group_test.go): интеграция
// uptime↔группы тестируется без фейков резолвера корней. Присваивается в
// Detector.IncidentGroups структурно (duck-typing groupHook, как
// fakeDepChecker в detector_test.go).
func newUptimeGrouper(pool *pgxpool.Pool) *incidentgroup.Grouper {
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: depsuppress.NewSuppressor(pool),
	}
}

// seedMonitorMonitorEdge — явное ребро зависимости monitor(parent) ->
// monitor(child) (B5, alert_dependencies).
func seedMonitorMonitorEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentMonitorID, childMonitorID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_monitor_id)
		VALUES ($1,$2,$3)`, projectID, parentMonitorID, childMonitorID); err != nil {
		t.Fatalf("seed monitor->monitor edge: %v", err)
	}
}

// seedMonitorHostEdge — явное ребро зависимости monitor(parent) ->
// host(child): host-инцидент под uptime-корнем (сценарий 2 брифа).
func seedMonitorHostEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentMonitorID, childHostID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parentMonitorID, childHostID); err != nil {
		t.Fatalf("seed monitor->host edge: %v", err)
	}
}

// seedGroupHost — хост проекта (пакет uptime своих host-хелперов не имеет;
// образец internal/incidentgroup/group_test.go).
func seedGroupHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'','') RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed host: %v", err)
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

// readUptimeGroupID — group_id uptime-инцидента (nil — вне групп).
func readUptimeGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read uptime group_id: %v", err)
	}
	return gid
}

// readHostGroupID — group_id host-инцидента (nil — вне групп).
func readHostGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM host_incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read host group_id: %v", err)
	}
	return gid
}

// readGroupRoot — (root_source, root_incident_id) группы.
func readGroupRoot(t *testing.T, pool *pgxpool.Pool, groupID int64) (string, int64) {
	t.Helper()
	var source string
	var rootInc int64
	if err := pool.QueryRow(context.Background(),
		`SELECT root_source, root_incident_id FROM incident_groups WHERE id = $1`, groupID).
		Scan(&source, &rootInc); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	return source, rootInc
}

// TestSettleHeldAttachesSuppressedChild — «uptime-членство только через
// MarkSuppressedByDep» (MAJOR-4): монитор-ребёнок за упавшим монитором-
// родителем (ребро monitor->monitor), открытый непронотифаенный инцидент
// ребёнка; settleHeldIncident на следующем тике → suppressed_by_dep=true И
// group_id указывает на группу корня-родителя. Уведомление ребёнка не
// уходит никогда (гейт B5).
func TestSettleHeldAttachesSuppressedChild(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	parent := createMonitor(t, svc, pid, 1, 1)
	child := createMonitor(t, svc, pid, 1, 1)
	seedMonitorMonitorEdge(t, pool, pid, parent.ID, child.ID)

	// Корень: открытый инцидент родителя, минуя детектор (его собственные
	// хуки — сценарии тестов ниже).
	parentInc, created, err := svc.OpenIncident(ctx, parent.ID, "root down", []string{"local"}, false)
	if err != nil || !created {
		t.Fatalf("open parent incident: created=%v err=%v", created, err)
	}

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: true}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second}
	d.IncidentGroups = newUptimeGrouper(pool)
	now := time.Now().UTC()

	// Тик 1: инцидент ребёнка открывается, "down" придержан (HasParent).
	applyAndDetect(t, ctx, svc, d, child, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, child.ID)
	if inc.NotifiedOpen {
		t.Fatal("NotifiedOpen = true, want false: у ребёнка задекларирован родитель, уведомление придержано")
	}

	// Тик 2: родитель down → settleHeldIncident подавляет и присоединяет.
	applyAndDetect(t, ctx, svc, d, child, "local", false, "boom", now.Add(time.Second), nil)
	inc = assertOpenIncident(t, ctx, svc, child.ID)
	if !inc.SuppressedByDep {
		t.Fatal("SuppressedByDep = false, want true: родитель down на втором тике")
	}
	gid := readUptimeGroupID(t, pool, inc.ID)
	if gid == nil {
		t.Fatal("group_id IS NULL — B5-подавленный ребёнок должен войти в состав группы корня")
	}
	source, rootInc := readGroupRoot(t, pool, *gid)
	if source != "uptime" || rootInc != parentInc.ID {
		t.Errorf("группа якорится на %s/%d, want uptime/%d (инцидент монитора-родителя)", source, rootInc, parentInc.ID)
	}
	if got := len(notifier.kindEvents("down")); got != 0 {
		t.Errorf("down events = %d, want 0: гейт уведомлений члена остаётся B5-шным", got)
	}
}

// TestUptimeRootRetroAttachesHostMember — корень (Р7): ребро
// monitor(parent)->host(child), открытый disk-инцидент хоста child, затем
// открытие uptime-инцидента родителя → OnRootOpened ретро-присоединяет
// disk-инцидент к группе свежего uptime-корня.
func TestUptimeRootRetroAttachesHostMember(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	parent := createMonitor(t, svc, pid, 1, 1)
	childHost := seedGroupHost(t, pool, pid, "web-01")
	seedMonitorHostEdge(t, pool, pid, parent.ID, childHost)
	memberInc := seedOpenDiskIncident(t, pool, pid, childHost)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	d.IncidentGroups = newUptimeGrouper(pool)

	// Монитор-родитель падает ПОСЛЕ того, как disk-инцидент ребёнка открыт.
	applyAndDetect(t, ctx, svc, d, parent, "local", false, "conn refused", time.Now().UTC(), nil)
	rootInc := assertOpenIncident(t, ctx, svc, parent.ID)

	gid := readHostGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("group_id IS NULL — открытие uptime-корня не ретро-присоединило уже открытый disk-инцидент ребёнка")
	}
	source, gotRoot := readGroupRoot(t, pool, *gid)
	if source != "uptime" || gotRoot != rootInc.ID {
		t.Errorf("группа якорится на %s/%d, want uptime/%d (свежеоткрытый инцидент монитора)", source, gotRoot, rootInc.ID)
	}
	// Ретро не трогает notified-статус члена, а корень (без своего родителя)
	// уведомляет как раньше.
	if got := len(notifier.kindEvents("down")); got != 1 {
		t.Errorf("down events = %d, want 1 (сам корень)", got)
	}
}

// TestUptimeRootCloseResolvesGroup — resolveIncident корня → группа resolved
// (Resolve через OnRootClosed).
func TestUptimeRootCloseResolvesGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	parent := createMonitor(t, svc, pid, 1, 1)
	childHost := seedGroupHost(t, pool, pid, "web-01")
	seedMonitorHostEdge(t, pool, pid, parent.ID, childHost)
	memberInc := seedOpenDiskIncident(t, pool, pid, childHost)

	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier}
	d.IncidentGroups = newUptimeGrouper(pool)
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, parent, "local", false, "conn refused", now, nil)
	rootInc := assertOpenIncident(t, ctx, svc, parent.ID)
	gid := readHostGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("setup: группа не создана при открытии корня")
	}

	// Монитор восстановился: resolveIncident закрывает инцидент и группу.
	applyAndDetect(t, ctx, svc, d, parent, "local", true, "", now.Add(time.Second), nil)
	assertNoOpenIncident(t, ctx, svc, parent.ID)

	var resolvedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT resolved_at FROM incident_groups WHERE id = $1`, *gid).Scan(&resolvedAt); err != nil {
		t.Fatalf("read group resolved_at: %v", err)
	}
	if resolvedAt == nil {
		t.Fatalf("группа %d (корень uptime/%d) не закрыта: resolved_at IS NULL после resolveIncident", *gid, rootInc.ID)
	}
}
