package depsuppress_test

import (
	"context"
	"errors"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestParentDownExplicit(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &hostID}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if down, err := sup.ParentDown(ctx, "host", hostID); err != nil || down {
		t.Fatalf("parent up → ParentDown = %v/%v, want false", down, err)
	}
	mustExec(t, pool, `INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now())`, monID)
	sup2 := depsuppress.NewSuppressor(pool)
	if down, err := sup2.ParentDown(ctx, "host", hostID); err != nil || !down {
		t.Fatalf("parent down → ParentDown = %v/%v, want true", down, err)
	}
	if has, err := sup2.HasParent(ctx, "host", hostID); err != nil || !has {
		t.Fatalf("HasParent = %v/%v, want true", has, err)
	}
}

func TestParentDownLabelAndTransitive(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, gwHost, monID := seedProjectHostMonitor(t, pool)
	_ = monID
	web := seedHost(t, pool, pid, "web1", "web", "prod")
	mustExec(t, pool, `INSERT INTO alert_dependencies (project_id, parent_host_id, child_label_scope, child_label_value)
		VALUES ($1,$2,'role','web')`, pid, gwHost)
	seedSilentIncident(t, pool, pid, gwHost)
	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if down, err := sup.ParentDown(ctx, "host", web); err != nil || !down {
		t.Fatalf("web-host под упавшим gw (по метке role=web) → ParentDown = %v/%v, want true", down, err)
	}
	if down, err := sup.ParentDown(ctx, "host", gwHost); err != nil || down {
		t.Fatalf("gw подавляет сам себя через self-match (баг MAJOR-5): ParentDown = %v/%v", down, err)
	}
}

func TestTransitiveChainViaOpenIntermediate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, b, monID := seedProjectHostMonitor(t, pool)
	_ = monID
	c := seedHost(t, pool, pid, "c", "db", "prod")
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentHostID: &b, ChildHostID: &c}); err != nil {
		t.Fatalf("create edge B->C: %v", err)
	}

	incID := seedSilentIncident(t, pool, pid, b)
	mustExec(t, pool, `UPDATE host_incidents SET suppressed_by_dep = true WHERE id = $1`, incID)

	sup := depsuppress.NewSuppressor(pool)
	if down, err := sup.ParentDown(context.Background(), "host", c); err != nil || !down {
		t.Fatalf("ParentDown(C) через открытый-но-подавленный B = %v/%v, want true", down, err)
	}
}

func TestParentDownReciprocalCycleNoBlackHole(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, _, monID := seedProjectHostMonitor(t, pool)
	a := seedHost(t, pool, pid, "a-web", "web", "prod")
	b := seedHost(t, pool, pid, "b-web", "web", "prod")

	mustExec(t, pool, `INSERT INTO alert_dependencies (project_id, parent_host_id, child_label_scope, child_label_value)
		VALUES ($1,$2,'role','web')`, pid, a)
	mustExec(t, pool, `INSERT INTO alert_dependencies (project_id, parent_host_id, child_label_scope, child_label_value)
		VALUES ($1,$2,'role','web')`, pid, b)
	seedSilentIncident(t, pool, pid, a)
	seedSilentIncident(t, pool, pid, b)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if down, err := sup.ParentDown(ctx, "host", a); err != nil || down {
		t.Fatalf("A во взаимном цикле НЕ должен подавляться (чёрная дыра): ParentDown(A) = %v/%v, want false", down, err)
	}
	if down, err := sup.ParentDown(ctx, "host", b); err != nil || down {
		t.Fatalf("B во взаимном цикле НЕ должен подавляться (чёрная дыра): ParentDown(B) = %v/%v, want false", down, err)
	}

	ctl := seedHost(t, pool, pid, "ctl", "db", "prod")
	if _, err := depsuppress.NewStore(pool).Create(ctx, depsuppress.Edge{
		ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &ctl}); err != nil {
		t.Fatalf("create control edge: %v", err)
	}
	mustExec(t, pool, `INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now())`, monID)
	sup2 := depsuppress.NewSuppressor(pool)
	if down, err := sup2.ParentDown(ctx, "host", ctl); err != nil || !down {
		t.Fatalf("контроль: explicit-цепочка обязана подавляться: ParentDown(ctl) = %v/%v, want true", down, err)
	}
}

func TestCheckIncidentHostVanished(t *testing.T) {
	pool := testenv.MigratedPG(t)
	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "host", 987654321); err != nil || hasParent || parentDown {
		t.Fatalf("CheckIncident для несуществующего инцидента = %v/%v/%v, want false/false/nil", hasParent, parentDown, err)
	}
}

// Мониторный аналог TestCheckIncidentHostSource: escalation.Scheduler держит
// один DepChecker на все source, и "uptime" обязан резолвиться так же честно,
// как "host" — иначе периодическое снятие подавления для uptime молчаливо
// решило бы «родителя нет» и снимало бы подавление сразу же на каждом тике.
func TestCheckIncidentUptimeSource(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentHostID: &hostID, ChildMonitorID: &monID}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	seedSilentIncident(t, pool, pid, hostID) // родительский host — down

	var incID int64
	mustScan(t, pool, &incID,
		`INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now()) RETURNING id`, monID)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "uptime", incID); err != nil || !hasParent || !parentDown {
		t.Fatalf("CheckIncident(uptime,%d) = %v/%v/%v, want true/true/nil", incID, hasParent, parentDown, err)
	}
}

func TestCheckIncidentUptimeVanished(t *testing.T) {
	pool := testenv.MigratedPG(t)
	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "uptime", 987654321); err != nil || hasParent || parentDown {
		t.Fatalf("CheckIncident для несуществующего uptime-инцидента = %v/%v/%v, want false/false/nil", hasParent, parentDown, err)
	}
}

func TestMarkSuppressed(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, _ := seedProjectHostMonitor(t, pool)
	incID := seedSilentIncident(t, pool, pid, hostID)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if err := sup.MarkSuppressed(ctx, "host", incID); err != nil {
		t.Fatalf("MarkSuppressed(host): %v", err)
	}
	var flag bool
	if err := pool.QueryRow(ctx, `SELECT suppressed_by_dep FROM host_incidents WHERE id=$1`, incID).Scan(&flag); err != nil {
		t.Fatalf("select suppressed_by_dep: %v", err)
	}
	if !flag {
		t.Fatal("suppressed_by_dep want true after MarkSuppressed")
	}

	if err := sup.MarkSuppressed(ctx, "monitor", incID); err != nil {
		t.Fatalf("MarkSuppressed(monitor): want nil (no-op), got %v", err)
	}
}

func TestParentDownLabelDoesNotCrossProjectBoundary(t *testing.T) {
	pool := testenv.MigratedPG(t)
	p1, gw, mon1 := seedProjectHostMonitor(t, pool)
	_ = mon1
	p1Web := seedHost(t, pool, p1, "p1-web", "web", "prod")
	p2, p2Web, _ := seedProjectHostMonitor(t, pool)
	_ = p2

	mustExec(t, pool, `INSERT INTO alert_dependencies (project_id, parent_host_id, child_label_scope, child_label_value)
		VALUES ($1,$2,'role','web')`, p1, gw)
	seedSilentIncident(t, pool, p1, gw)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()

	if down, err := sup.ParentDown(ctx, "host", p2Web); err != nil || down {
		t.Fatalf("хост ЧУЖОГО проекта P2 не должен подавляться label-ребром P1: ParentDown = %v/%v, want false", down, err)
	}
	if down, err := sup.ParentDown(ctx, "host", p1Web); err != nil || !down {
		t.Fatalf("контроль: хост СВОЕГО проекта P1 обязан подавляться: ParentDown = %v/%v, want true", down, err)
	}
}

func TestCheckIncidentHostSource(t *testing.T) {
	pool := testenv.MigratedPG(t)
	pid, hostID, monID := seedProjectHostMonitor(t, pool)
	if _, err := depsuppress.NewStore(pool).Create(context.Background(), depsuppress.Edge{
		ProjectID: pid, ParentMonitorID: &monID, ChildHostID: &hostID}); err != nil {
		t.Fatalf("create edge: %v", err)
	}
	mustExec(t, pool, `INSERT INTO incidents (monitor_id, started_at) VALUES ($1, now())`, monID)

	incID := seedSilentIncident(t, pool, pid, hostID)

	sup := depsuppress.NewSuppressor(pool)
	ctx := context.Background()
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "host", incID); err != nil || !hasParent || !parentDown {
		t.Fatalf("CheckIncident(host,%d) = %v/%v/%v, want true/true/nil", incID, hasParent, parentDown, err)
	}
	// "monitor" — не зарегистрированный source (это kind, не source): список
	// источников не закрыт, незнакомый обязан быть ошибкой, не тихим false.
	if hasParent, parentDown, err := sup.CheckIncident(ctx, "monitor", 999999); !errors.Is(err, depsuppress.ErrUnknownSource) || hasParent || parentDown {
		t.Fatalf("CheckIncident(monitor,...) = %v/%v/%v, want false/false/ErrUnknownSource", hasParent, parentDown, err)
	}
}
