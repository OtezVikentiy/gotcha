package incidentgroup_test

import (
	"context"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestSweepClosesGroupWhenRootDeleted(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	store := incidentgroup.NewStore(pool)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := store.SetGroup(ctx, "host", memberInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Член открытой группы невидим эскалации — точка отсчёта для ассерта ниже.
	svc := host.NewIncidentService(pool)
	pending, err := svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked before sweep: %v", err)
	}
	for _, p := range pending {
		if p.ID == memberInc {
			t.Fatalf("member %d must be hidden from OpenUnacked while group is open", memberInc)
		}
	}

	// Удаляем хост-корень: каскад 0066 сносит его инциденты, группа сиротеет.
	mustExec(t, pool, `DELETE FROM hosts WHERE id = $1`, rootHost)

	n, err := incidentgroup.SweepOrphanGroups(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	var resolved *time.Time
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM incident_groups WHERE id = $1`, g.ID).Scan(&resolved); err != nil {
		t.Fatalf("read group: %v", err)
	}
	if resolved == nil {
		t.Fatalf("group must be resolved by sweep")
	}
	// Член снова виден эскалации (досылается step0): host.OpenUnacked.
	pending, err = svc.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked: %v", err)
	}
	found := false
	for _, p := range pending {
		if p.ID == memberInc {
			found = true
		}
	}
	if !found {
		t.Fatalf("freed member %d must be back in OpenUnacked", memberInc)
	}
}

func TestSweepClosesGroupWhenRootResolvedWithoutHook(t *testing.T) {
	// Корень закрыт мимо хука (ResolveOpenByProjectKind — выключение порога
	// оператором): sweep подхватывает. Контроль: группа живого корня не тронута.
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	aliveHost := seedHost(t, pool, projectID, "alive-"+randSlug(t))
	aliveInc := seedSilent(t, pool, projectID, aliveHost, true)

	store := incidentgroup.NewStore(pool)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	gAlive, err := store.EnsureGroup(ctx, projectID, "host", aliveInc, "host", aliveHost)
	if err != nil {
		t.Fatalf("EnsureGroup alive: %v", err)
	}

	mustExec(t, pool,
		`UPDATE host_incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, rootInc)

	n, err := incidentgroup.SweepOrphanGroups(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	var resolved *time.Time
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM incident_groups WHERE id = $1`, g.ID).Scan(&resolved); err != nil {
		t.Fatalf("read group: %v", err)
	}
	if resolved == nil {
		t.Fatalf("group of resolved root must be closed by sweep")
	}
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM incident_groups WHERE id = $1`, gAlive.ID).Scan(&resolved); err != nil {
		t.Fatalf("read alive group: %v", err)
	}
	if resolved != nil {
		t.Fatalf("group of open root must not be touched by sweep")
	}
}

func TestSweepClosesGroupWhenUptimeRootResolved(t *testing.T) {
	// Ветка root_source='uptime' той же врезки: открытый uptime-корень группу
	// держит, закрытый (мимо хука OnRootClosed) — отдаёт sweep'у.
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	var monitorID, rootInc int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'m1','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &rootInc,
		`INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)

	store := incidentgroup.NewStore(pool)
	g, err := store.EnsureGroup(ctx, projectID, "uptime", rootInc, "monitor", monitorID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	n, err := incidentgroup.SweepOrphanGroups(ctx, pool)
	if err != nil || n != 0 {
		t.Fatalf("sweep with open root: n=%d err=%v, want 0", n, err)
	}

	mustExec(t, pool, `UPDATE incidents SET resolved_at = now() WHERE id = $1`, rootInc)

	n, err = incidentgroup.SweepOrphanGroups(ctx, pool)
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v, want 1", n, err)
	}
	var resolved *time.Time
	if err := pool.QueryRow(ctx, `SELECT resolved_at FROM incident_groups WHERE id = $1`, g.ID).Scan(&resolved); err != nil {
		t.Fatalf("read group: %v", err)
	}
	if resolved == nil {
		t.Fatalf("group of resolved uptime root must be closed by sweep")
	}
}

func TestPurgeOldGroups(t *testing.T) {
	// resolved-группа старше ретеншена удаляется, открытая — не тронута.
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	oldHost := seedHost(t, pool, projectID, "old-"+randSlug(t))
	oldInc := seedSilent(t, pool, projectID, oldHost, true)
	openHost := seedHost(t, pool, projectID, "open-"+randSlug(t))
	openInc := seedSilent(t, pool, projectID, openHost, true)

	store := incidentgroup.NewStore(pool)
	gOld, err := store.EnsureGroup(ctx, projectID, "host", oldInc, "host", oldHost)
	if err != nil {
		t.Fatalf("EnsureGroup old: %v", err)
	}
	gOpen, err := store.EnsureGroup(ctx, projectID, "host", openInc, "host", openHost)
	if err != nil {
		t.Fatalf("EnsureGroup open: %v", err)
	}
	mustExec(t, pool,
		`UPDATE incident_groups SET resolved_at = now() - interval '48 hours' WHERE id = $1`, gOld.ID)

	n, err := incidentgroup.PurgeOldGroups(ctx, pool, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v, want 1", n, err)
	}
	var cnt int64
	mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, gOld.ID)
	if cnt != 0 {
		t.Fatalf("old resolved group must be deleted")
	}
	mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, gOpen.ID)
	if cnt != 1 {
		t.Fatalf("open group must survive purge")
	}
}
