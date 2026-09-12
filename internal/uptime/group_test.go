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

func newUptimeGrouper(pool *pgxpool.Pool) *incidentgroup.Grouper {
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: depsuppress.NewSuppressor(pool),
	}
}

func seedMonitorMonitorEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentMonitorID, childMonitorID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_monitor_id)
		VALUES ($1,$2,$3)`, projectID, parentMonitorID, childMonitorID); err != nil {
		t.Fatalf("seed monitor->monitor edge: %v", err)
	}
}

func seedMonitorHostEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentMonitorID, childHostID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parentMonitorID, childHostID); err != nil {
		t.Fatalf("seed monitor->host edge: %v", err)
	}
}

func seedHostMonitorEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentHostID, childMonitorID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_host_id, child_monitor_id)
		VALUES ($1,$2,$3)`, projectID, parentHostID, childMonitorID); err != nil {
		t.Fatalf("seed host->monitor edge: %v", err)
	}
}

func seedOpenSilentIncident(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',true) RETURNING id`,
		projectID, hostID).Scan(&id); err != nil {
		t.Fatalf("seed silent incident: %v", err)
	}
	return id
}

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

func readUptimeGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read uptime group_id: %v", err)
	}
	return gid
}

func readHostGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM host_incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read host group_id: %v", err)
	}
	return gid
}

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

func TestSettleHeldAttachesSuppressedChild(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	parent := createMonitor(t, svc, pid, 1, 1)
	child := createMonitor(t, svc, pid, 1, 1)
	seedMonitorMonitorEdge(t, pool, pid, parent.ID, child.ID)

	parentInc, created, err := svc.OpenIncident(ctx, parent.ID, "root down", []string{"local"}, false)
	if err != nil || !created {
		t.Fatalf("open parent incident: created=%v err=%v", created, err)
	}

	notifier := &fakeNotifier{}
	dep := &fakeDepChecker{hasParent: true, parentDown: true}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: dep, SettleGrace: 20 * time.Second, Pool: pool}
	d.IncidentGroups = newUptimeGrouper(pool)
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, child, "local", false, "boom", now, nil)
	inc := assertOpenIncident(t, ctx, svc, child.ID)
	if inc.NotifiedOpen {
		t.Fatal("NotifiedOpen = true, want false: у ребёнка задекларирован родитель, уведомление придержано")
	}

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
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	d.IncidentGroups = newUptimeGrouper(pool)

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
	if got := len(notifier.kindEvents("down")); got != 1 {
		t.Errorf("down events = %d, want 1 (сам корень)", got)
	}
}

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
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Pool: pool}
	d.IncidentGroups = newUptimeGrouper(pool)
	now := time.Now().UTC()

	applyAndDetect(t, ctx, svc, d, parent, "local", false, "conn refused", now, nil)
	rootInc := assertOpenIncident(t, ctx, svc, parent.ID)
	gid := readHostGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("setup: группа не создана при открытии корня")
	}

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

func TestOpenIncidentCrossSpeciesRootAttachesGrandchild(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := uptime.NewService(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pid := newProject(t, pool)
	a := seedGroupHost(t, pool, pid, "gw-01")
	mon := createMonitor(t, svc, pid, 1, 1)
	c := seedGroupHost(t, pool, pid, "web-01")
	seedHostMonitorEdge(t, pool, pid, a, mon.ID)
	seedMonitorHostEdge(t, pool, pid, mon.ID, c)

	rootInc := seedOpenSilentIncident(t, pool, pid, a)
	memberInc := seedOpenDiskIncident(t, pool, pid, c)

	// один Suppressor и на Detector.Dep, и на Grouper.Roots — как в проде;
	// иначе DownRoot внутри openIncident не увидит A упавшим (разный кеш).
	sup := depsuppress.NewSuppressor(pool)
	notifier := &fakeNotifier{}
	d := &uptime.Detector{Svc: svc, Notifier: notifier, Dep: sup, Pool: pool}
	d.IncidentGroups = &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: sup,
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "conn refused", time.Now().UTC(), nil)
	assertOpenIncident(t, ctx, svc, mon.ID)

	gid := readHostGroupID(t, pool, memberInc)
	if gid == nil {
		t.Fatal("group_id IS NULL — падение M под host-корнем A не ретро-присоединило C")
	}
	source, gotRoot := readGroupRoot(t, pool, *gid)
	if source != "host" || gotRoot != rootInc {
		t.Errorf("группа якорится на %s/%d, want фактический корень каскада — host/%d (инцидент хоста A)", source, gotRoot, rootInc)
	}

	applyAndDetect(t, ctx, svc, d, mon, "local", false, "conn refused", time.Now().UTC().Add(time.Second), nil)
	monInc := assertOpenIncident(t, ctx, svc, mon.ID)
	if !monInc.SuppressedByDep {
		t.Fatal("SuppressedByDep(M) = false, want true: родитель A down на тике 2")
	}
	monGid := readUptimeGroupID(t, pool, monInc.ID)
	if monGid == nil || *monGid != *gid {
		t.Errorf("M.group_id = %v, want %d (та же группа host-корня A, что и у C)", monGid, *gid)
	}
}
