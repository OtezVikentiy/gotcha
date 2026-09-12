package incidentgroup_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
	if _, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

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

	// Удаляем хост-корень — каскад сносит его инциденты, группа сиротеет.
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

func TestJanitorRunSweepsOrphanGroups(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	store := incidentgroup.NewStore(pool)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	mustExec(t, pool, `DELETE FROM hosts WHERE id = $1`, rootHost)

	j := &incidentgroup.Janitor{Pool: pool, SweepInterval: 10 * time.Millisecond}
	go j.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	var resolved *time.Time
	for time.Now().Before(deadline) {
		if err := pool.QueryRow(ctx, `SELECT resolved_at FROM incident_groups WHERE id = $1`, g.ID).Scan(&resolved); err != nil {
			t.Fatalf("read group: %v", err)
		}
		if resolved != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resolved == nil {
		t.Fatalf("Janitor.Run must sweep the orphan group via its ticker within the deadline")
	}
}

func TestPurgeOldGroups(t *testing.T) {
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

func TestPurgeOldGroupsSurvivesOpenMember(t *testing.T) {
	cases := []struct {
		name   string
		source string
		seed   func(t *testing.T, pool *pgxpool.Pool, projectID int64) int64
		close_ func(t *testing.T, pool *pgxpool.Pool, memberID int64)
	}{
		{
			name:   "host",
			source: "host",
			seed: func(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
				memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
				var id int64
				mustScan(t, pool, &id, `
					INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
					VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
				return id
			},
			close_: func(t *testing.T, pool *pgxpool.Pool, memberID int64) {
				mustExec(t, pool,
					`UPDATE host_incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, memberID)
			},
		},
		{
			name:   "uptime",
			source: "uptime",
			seed: func(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
				var monitorID, id int64
				mustScan(t, pool, &monitorID, `
					INSERT INTO monitors (project_id, name, kind, interval_seconds)
					VALUES ($1,'m-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
				mustScan(t, pool, &id,
					`INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)
				return id
			},
			close_: func(t *testing.T, pool *pgxpool.Pool, memberID int64) {
				mustExec(t, pool, `UPDATE incidents SET resolved_at = now() WHERE id = $1`, memberID)
			},
		},
		{
			name:   "metric",
			source: "metric",
			seed: func(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
				var ruleID, id int64
				mustScan(t, pool, &ruleID, `
					INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
					VALUES ($1,'cpu.load','avg','gt',0.9) RETURNING id`, projectID)
				mustScan(t, pool, &id, `
					INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
					VALUES ($1,$2,1,1) RETURNING id`, ruleID, projectID)
				return id
			},
			close_: func(t *testing.T, pool *pgxpool.Pool, memberID int64) {
				mustExec(t, pool,
					`UPDATE metric_incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, memberID)
			},
		},
		{
			name:   "slo",
			source: "slo",
			seed: func(t *testing.T, pool *pgxpool.Pool, projectID int64) int64 {
				var sloID, id int64
				mustScan(t, pool, &sloID, `
					INSERT INTO slos (project_id, name, sli_kind, target, window_days)
					VALUES ($1,'slo-`+randSlug(t)+`','availability',0.99,30) RETURNING id`, projectID)
				mustScan(t, pool, &id, `
					INSERT INTO slo_incidents (slo_id, project_id, burn_rate)
					VALUES ($1,$2,20) RETURNING id`, sloID, projectID)
				return id
			},
			close_: func(t *testing.T, pool *pgxpool.Pool, memberID int64) {
				mustExec(t, pool,
					`UPDATE slo_incidents SET status = 'resolved', resolved_at = now() WHERE id = $1`, memberID)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testenv.MigratedPG(t)
			ctx := context.Background()
			projectID := seedProject(t, pool)
			rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
			rootInc := seedSilent(t, pool, projectID, rootHost, true)

			store := incidentgroup.NewStore(pool)
			g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
			if err != nil {
				t.Fatalf("EnsureGroup: %v", err)
			}
			memberID := tc.seed(t, pool, projectID)
			if _, err := store.SetGroup(ctx, projectID, tc.source, memberID, g.ID); err != nil {
				t.Fatalf("SetGroup %s: %v", tc.source, err)
			}

			mustExec(t, pool,
				`UPDATE incident_groups SET resolved_at = now() - interval '48 hours' WHERE id = $1`, g.ID)

			n, err := incidentgroup.PurgeOldGroups(ctx, pool, 24*time.Hour)
			if err != nil || n != 0 {
				t.Fatalf("purge with open %s member: n=%d err=%v, want 0", tc.source, n, err)
			}
			var cnt int64
			mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, g.ID)
			if cnt != 1 {
				t.Fatalf("group with open %s member must survive purge", tc.source)
			}

			tc.close_(t, pool, memberID)

			n, err = incidentgroup.PurgeOldGroups(ctx, pool, 24*time.Hour)
			if err != nil || n != 1 {
				t.Fatalf("purge after %s member closed: n=%d err=%v, want 1", tc.source, n, err)
			}
			mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, g.ID)
			if cnt != 0 {
				t.Fatalf("group must be purged once %s member is closed", tc.source)
			}
		})
	}
}

func TestPurgeOldGroupsRetentionBoundary(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	youngHost := seedHost(t, pool, projectID, "young-"+randSlug(t))
	youngInc := seedSilent(t, pool, projectID, youngHost, true)
	oldHost := seedHost(t, pool, projectID, "old-"+randSlug(t))
	oldInc := seedSilent(t, pool, projectID, oldHost, true)

	store := incidentgroup.NewStore(pool)
	gYoung, err := store.EnsureGroup(ctx, projectID, "host", youngInc, "host", youngHost)
	if err != nil {
		t.Fatalf("EnsureGroup young: %v", err)
	}
	gOld, err := store.EnsureGroup(ctx, projectID, "host", oldInc, "host", oldHost)
	if err != nil {
		t.Fatalf("EnsureGroup old: %v", err)
	}
	mustExec(t, pool,
		`UPDATE incident_groups SET resolved_at = now() - interval '1 minute' WHERE id = $1`, gYoung.ID)
	mustExec(t, pool,
		`UPDATE incident_groups SET resolved_at = now() - interval '2 hours' WHERE id = $1`, gOld.ID)

	n, err := incidentgroup.PurgeOldGroups(ctx, pool, time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v, want 1", n, err)
	}
	var cnt int64
	mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, gYoung.ID)
	if cnt != 1 {
		t.Fatalf("group resolved younger than retention must survive")
	}
	mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, gOld.ID)
	if cnt != 0 {
		t.Fatalf("group resolved older than retention must be purged")
	}
}

func TestJanitorRunPurgesThenRespectsRetentionEvery(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	host1 := seedHost(t, pool, projectID, "root1-"+randSlug(t))
	inc1 := seedSilent(t, pool, projectID, host1, true)
	g1, err := store.EnsureGroup(ctx, projectID, "host", inc1, "host", host1)
	if err != nil {
		t.Fatalf("EnsureGroup g1: %v", err)
	}
	mustExec(t, pool,
		`UPDATE incident_groups SET resolved_at = now() - interval '48 hours' WHERE id = $1`, g1.ID)

	j := &incidentgroup.Janitor{Pool: pool, SweepInterval: 10 * time.Millisecond, Retention: 24 * time.Hour}
	go j.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	var cnt int64
	for time.Now().Before(deadline) {
		mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, g1.ID)
		if cnt == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cnt != 0 {
		t.Fatalf("Janitor.Run must purge the old group on its first tick despite zero-value lastPurge")
	}

	host2 := seedHost(t, pool, projectID, "root2-"+randSlug(t))
	inc2 := seedSilent(t, pool, projectID, host2, true)
	g2, err := store.EnsureGroup(ctx, projectID, "host", inc2, "host", host2)
	if err != nil {
		t.Fatalf("EnsureGroup g2: %v", err)
	}
	mustExec(t, pool,
		`UPDATE incident_groups SET resolved_at = now() - interval '48 hours' WHERE id = $1`, g2.ID)

	time.Sleep(300 * time.Millisecond)
	mustScan(t, pool, &cnt, `SELECT count(*) FROM incident_groups WHERE id = $1`, g2.ID)
	if cnt != 1 {
		t.Fatalf("Janitor.Run must not re-run retention before retentionEvery elapses, group2 count=%d", cnt)
	}
}
