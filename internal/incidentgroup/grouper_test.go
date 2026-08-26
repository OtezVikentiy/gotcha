package incidentgroup_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// seedEdgeHH — явное ребро host(parent) -> host(child).
func seedEdgeHH(t *testing.T, pool *pgxpool.Pool, projectID, parent, child int64) {
	t.Helper()
	mustExec(t, pool, `
		INSERT INTO alert_dependencies (project_id, parent_host_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parent, child)
}

func newGrouper(pool *pgxpool.Pool) (*incidentgroup.Grouper, *depsuppress.Suppressor) {
	sup := depsuppress.NewSuppressor(pool)
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: sup,
	}, sup
}

func TestAttachMemberUnderInformingRoot(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)
	rootInc := seedSilent(t, pool, projectID, rootHost, true) // информирующий

	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, childHost)

	g, _ := newGrouper(pool)
	attached, informing, err := g.Attach(ctx, "host", memberInc, "host", childHost)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !attached || !informing {
		t.Fatalf("want attached under informing root, got attached=%v informing=%v", attached, informing)
	}
	var groupID int64
	mustScan(t, pool, &groupID, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc)
	var gotRootInc int64
	mustScan(t, pool, &gotRootInc, `SELECT root_incident_id FROM incident_groups WHERE id = $1`, groupID)
	if gotRootInc != rootInc {
		t.Fatalf("group must anchor to root incident %d, got %d", rootInc, gotRootInc)
	}
}

func TestAttachSilentRoot(t *testing.T) {
	// Немой корень (MAJOR-3): notified_open=false → attach для состава,
	// но informing=false — член уведомляет сам.
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)
	seedSilent(t, pool, projectID, rootHost, false) // немой

	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, childHost)

	g, _ := newGrouper(pool)
	attached, informing, err := g.Attach(ctx, "host", memberInc, "host", childHost)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !attached || informing {
		t.Fatalf("silent root: want attached=true informing=false, got %v/%v", attached, informing)
	}
}

func TestAttachSelfRootAndNoRoot(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	g, _ := newGrouper(pool)
	// Сам корень — не член собственной группы.
	attached, _, err := g.Attach(ctx, "host", rootInc, "host", rootHost)
	if err != nil || attached {
		t.Fatalf("self root must not attach: attached=%v err=%v", attached, err)
	}
	// Узел без упавшего корня — вне групп.
	lonely := seedHost(t, pool, projectID, "lonely-"+randSlug(t))
	var lonelyInc int64
	mustScan(t, pool, &lonelyInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, lonely)
	attached, _, err = g.Attach(ctx, "host", lonelyInc, "host", lonely)
	if err != nil || attached {
		t.Fatalf("node without down root must not attach: attached=%v err=%v", attached, err)
	}
}

func TestAttachCrossTenantIsolation(t *testing.T) {
	// Узлы чужого проекта не матчатся: ребро и упавший корень в проекте A
	// не притягивают инцидент одноимённого хоста проекта B (изоляция уже
	// обеспечена явными id рёбер + project_id label-матча B5; тест — сторож).
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projA := seedProject(t, pool)
	projB := seedProject(t, pool)
	rootA := seedHost(t, pool, projA, "gw-"+randSlug(t))
	childA := seedHost(t, pool, projA, "web-shared")
	_ = childA
	seedEdgeHH(t, pool, projA, rootA, childA)
	seedSilent(t, pool, projA, rootA, true)

	hostB := seedHost(t, pool, projB, "web-shared-b-"+randSlug(t))
	var incB int64
	mustScan(t, pool, &incB, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projB, hostB)

	g, _ := newGrouper(pool)
	attached, _, err := g.Attach(ctx, "host", incB, "host", hostB)
	if err != nil || attached {
		t.Fatalf("cross-tenant attach must not happen: attached=%v err=%v", attached, err)
	}
}

func TestOnRootOpenedRetro(t *testing.T) {
	// Ретро (Р7): disk-алерт ребёнка опередил смерть корня → при открытии
	// корня присоединяется задним числом; notified_open члена не трогается.
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)

	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'disk','open',0,0,'',true) RETURNING id`, projectID, childHost)

	// Ребёнок тоже замолчал (иначе он не «на цепочке упавших» — узел члена
	// с открытым disk-инцидентом жив; членство идёт через down-родителя).
	// Для ретро по disk-инциденту ребёнка достаточно, чтобы у ЕГО узла
	// down-корнем был root: childHost жив, но его родитель rootHost упал.
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	g, _ := newGrouper(pool)
	if err := g.OnRootOpened(ctx, "host", rootInc, "host", rootHost, projectID); err != nil {
		t.Fatalf("OnRootOpened: %v", err)
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc).Scan(&groupID); err != nil {
		t.Fatalf("read member: %v", err)
	}
	if groupID == nil {
		t.Fatalf("member must be retro-attached")
	}
	var notified bool
	if err := pool.QueryRow(ctx, `SELECT notified_open FROM host_incidents WHERE id = $1`, memberInc).Scan(&notified); err != nil {
		t.Fatalf("read notified: %v", err)
	}
	if !notified {
		t.Fatalf("retro attach must not touch notified_open")
	}
}

func TestOnRootOpenedLazyNoMembers(t *testing.T) {
	// Нет членов — нет группы (ленивое создание, «пустых групп не бывает»).
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	g, _ := newGrouper(pool)
	if err := g.OnRootOpened(ctx, "host", rootInc, "host", rootHost, projectID); err != nil {
		t.Fatalf("OnRootOpened: %v", err)
	}
	var n int64
	mustScan(t, pool, &n, `SELECT count(*) FROM incident_groups WHERE root_incident_id = $1 AND root_source = 'host'`, rootInc)
	if n != 0 {
		t.Fatalf("no members -> no group, got %d groups", n)
	}
}
