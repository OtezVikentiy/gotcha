package incidentgroup_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func TestFeedComposition(t *testing.T) {
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

	memberHostName := "hostmember-" + randSlug(t)
	memberHost := seedHost(t, pool, projectID, memberHostName)
	var hostMemberInc int64
	mustScan(t, pool, &hostMemberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
	if _, err := store.SetGroup(ctx, projectID, "host", hostMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup host: %v", err)
	}

	var monitorID, uptimeMemberInc int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &uptimeMemberInc, `
		INSERT INTO incidents (monitor_id, notified_open, suppressed_by_dep)
		VALUES ($1,true,true) RETURNING id`, monitorID)
	if _, err := store.SetGroup(ctx, projectID, "uptime", uptimeMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup uptime: %v", err)
	}

	var ruleID, metricMemberInc int64
	mustScan(t, pool, &ruleID, `
		INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		VALUES ($1,'cpu.load','avg','gt',0.9) RETURNING id`, projectID)
	mustScan(t, pool, &metricMemberInc, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1,$2,1,1) RETURNING id`, ruleID, projectID)
	if _, err := store.SetGroup(ctx, projectID, "metric", metricMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup metric: %v", err)
	}

	var sloID, sloMemberInc int64
	mustScan(t, pool, &sloID, `
		INSERT INTO slos (project_id, name, sli_kind, target, window_days)
		VALUES ($1,'slo-`+randSlug(t)+`','availability',0.99,30) RETURNING id`, projectID)
	mustScan(t, pool, &sloMemberInc, `
		INSERT INTO slo_incidents (slo_id, project_id, burn_rate)
		VALUES ($1,$2,20) RETURNING id`, sloID, projectID)
	if _, err := store.SetGroup(ctx, projectID, "slo", sloMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup slo: %v", err)
	}

	members, err := store.Composition(ctx, projectID, g.ID)
	if err != nil {
		t.Fatalf("Composition: %v", err)
	}
	if len(members) != 4 {
		t.Fatalf("Composition len = %d, want 4", len(members))
	}
	bySource := map[string]incidentgroup.FeedItem{}
	for _, m := range members {
		bySource[m.Source] = m
	}
	h, ok := bySource["host"]
	if !ok {
		t.Fatalf("Composition missing host member")
	}
	if h.IncidentID != hostMemberInc || h.SuppressedByDep || h.Title != memberHostName {
		t.Fatalf("host member: IncidentID=%d Title=%q SuppressedByDep=%v, want %d/%q/false",
			h.IncidentID, h.Title, h.SuppressedByDep, hostMemberInc, memberHostName)
	}
	if !h.HeldByGroup {
		t.Fatalf("host member HeldByGroup = false, want true (informing root, open group, open member)")
	}

	u, ok := bySource["uptime"]
	if !ok {
		t.Fatalf("Composition missing uptime member")
	}
	if u.IncidentID != uptimeMemberInc || !u.SuppressedByDep {
		t.Fatalf("uptime member: IncidentID=%d SuppressedByDep=%v, want %d/true", u.IncidentID, u.SuppressedByDep, uptimeMemberInc)
	}
	if !u.HeldByGroup {
		t.Fatalf("uptime member HeldByGroup = false, want true (independent of SuppressedByDep=true)")
	}

	mm, ok := bySource["metric"]
	if !ok {
		t.Fatalf("Composition missing metric member")
	}
	if mm.IncidentID != metricMemberInc || mm.Title != "cpu.load" {
		t.Fatalf("metric member: IncidentID=%d Title=%q, want %d/cpu.load", mm.IncidentID, mm.Title, metricMemberInc)
	}
	if !mm.HeldByGroup {
		t.Fatalf("metric member HeldByGroup = false, want true (informing root, open group, open member)")
	}

	s, ok := bySource["slo"]
	if !ok {
		t.Fatalf("Composition missing slo member")
	}
	if s.IncidentID != sloMemberInc {
		t.Fatalf("slo member: IncidentID=%d, want %d", s.IncidentID, sloMemberInc)
	}
	if !s.HeldByGroup {
		t.Fatalf("slo member HeldByGroup = false, want true (informing root, open group, open member)")
	}
}

func TestFeedCompositionHeldByGroupGates(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	store := incidentgroup.NewStore(pool)

	t.Run("silent root", func(t *testing.T) {
		projectID := seedProject(t, pool)
		rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
		rootInc := seedSilent(t, pool, projectID, rootHost, false) // немой корень
		g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
		if err != nil {
			t.Fatalf("EnsureGroup: %v", err)
		}
		memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
		var memberInc int64
		mustScan(t, pool, &memberInc, `
			INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
			VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
		if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
			t.Fatalf("SetGroup: ok=%v err=%v", ok, err)
		}
		members, err := store.Composition(ctx, projectID, g.ID)
		if err != nil {
			t.Fatalf("Composition: %v", err)
		}
		for _, m := range members {
			if m.IncidentID == memberInc && m.HeldByGroup {
				t.Fatalf("member of a silent (notified_open=false) root must have HeldByGroup=false: %+v", m)
			}
		}
	})

	t.Run("member already resolved", func(t *testing.T) {
		projectID := seedProject(t, pool)
		rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
		rootInc := seedSilent(t, pool, projectID, rootHost, true) // информирующий корень
		g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
		if err != nil {
			t.Fatalf("EnsureGroup: %v", err)
		}
		memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
		var memberInc int64
		mustScan(t, pool, &memberInc, `
			INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, resolved_at)
			VALUES ($1,$2,'disk','resolved',0,0,'', now()) RETURNING id`, projectID, memberHost)
		if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
			t.Fatalf("SetGroup: ok=%v err=%v", ok, err)
		}
		members, err := store.Composition(ctx, projectID, g.ID)
		if err != nil {
			t.Fatalf("Composition: %v", err)
		}
		for _, m := range members {
			if m.IncidentID == memberInc && m.HeldByGroup {
				t.Fatalf("a member that has already resolved must have HeldByGroup=false (its notification silence is history, not current state): %+v", m)
			}
		}
	})

	t.Run("group resolved", func(t *testing.T) {
		projectID := seedProject(t, pool)
		rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
		rootInc := seedSilent(t, pool, projectID, rootHost, true) // информирующий корень
		g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
		if err != nil {
			t.Fatalf("EnsureGroup: %v", err)
		}
		memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
		var memberInc int64
		mustScan(t, pool, &memberInc, `
			INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
			VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
		if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
			t.Fatalf("SetGroup: ok=%v err=%v", ok, err)
		}
		if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
			t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
		}
		members, err := store.Composition(ctx, projectID, g.ID)
		if err != nil {
			t.Fatalf("Composition: %v", err)
		}
		for _, m := range members {
			if m.IncidentID == memberInc && m.HeldByGroup {
				t.Fatalf("a member of an already-resolved group must have HeldByGroup=false: %+v", m)
			}
		}
	})
}

func TestFeedOpenOutOfGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	groupedHost := seedHost(t, pool, projectID, "grouped-"+randSlug(t))
	var groupedInc int64
	mustScan(t, pool, &groupedInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, groupedHost)
	if _, err := store.SetGroup(ctx, projectID, "host", groupedInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	loneHost := seedHost(t, pool, projectID, "lone-"+randSlug(t))
	var loneInc int64
	mustScan(t, pool, &loneInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'memory','open',0,0,'') RETURNING id`, projectID, loneHost)

	var traceInc int64
	mustScan(t, pool, &traceInc, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1,'endpoint_p95','/api/x','duration',100,500,500) RETURNING id`, projectID)

	items, err := store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup: %v", err)
	}
	var haveLone, haveTrace, haveGrouped, haveRoot bool
	for _, it := range items {
		switch {
		case it.Source == "host" && it.IncidentID == loneInc:
			haveLone = true
		case it.Source == "trace" && it.IncidentID == traceInc:
			haveTrace = true
		case it.Source == "host" && it.IncidentID == groupedInc:
			haveGrouped = true
		case it.Source == "host" && it.IncidentID == rootInc:
			haveRoot = true
		}
	}
	if !haveLone {
		t.Fatalf("OpenOutOfGroup must contain lone host incident %d", loneInc)
	}
	if !haveTrace {
		t.Fatalf("OpenOutOfGroup must contain trace regression %d", traceInc)
	}
	if haveGrouped {
		t.Fatalf("OpenOutOfGroup must NOT contain grouped member %d", groupedInc)
	}
	if haveRoot {
		t.Fatalf("OpenOutOfGroup must NOT contain the group's own root incident %d (shown in the group card header)", rootInc)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM incident_groups WHERE id = $1`, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}
	items, err = store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup after group purge: %v", err)
	}
	haveRoot = false
	for _, it := range items {
		if it.Source == "host" && it.IncidentID == rootInc {
			haveRoot = true
		}
	}
	if !haveRoot {
		t.Fatalf("OpenOutOfGroup must contain root incident %d once its group is purged", rootInc)
	}
}

func TestFeedRootNotDuplicatedUptime(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	var monitorID, rootInc int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &rootInc, `
		INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)
	if _, err := store.EnsureGroup(ctx, projectID, "uptime", rootInc, "monitor", monitorID); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	items, err := store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup: %v", err)
	}
	for _, it := range items {
		if it.Source == "uptime" && it.IncidentID == rootInc {
			t.Fatalf("OpenOutOfGroup must NOT contain uptime root incident %d", rootInc)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE incidents SET resolved_at = now() WHERE id = $1`, rootInc); err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := store.Resolve(ctx, "uptime", rootInc); err != nil {
		t.Fatalf("Resolve group: %v", err)
	}
	closed, err := store.ClosedSince(ctx, projectID, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ClosedSince: %v", err)
	}
	var haveRoot bool
	for _, it := range closed {
		if it.Source == "uptime" && it.IncidentID == rootInc {
			haveRoot = true
		}
	}
	if !haveRoot {
		t.Fatalf("ClosedSince must contain closed uptime root incident %d once its group is resolved (R4 fix)", rootInc)
	}
}

func TestFeedClosedSince(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	groupedHost := seedHost(t, pool, projectID, "grouped-"+randSlug(t))
	var groupedInc int64
	mustScan(t, pool, &groupedInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, resolved_at)
		VALUES ($1,$2,'disk','resolved',0,0,'', now()) RETURNING id`, projectID, groupedHost)
	if _, err := store.SetGroup(ctx, projectID, "host", groupedInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	loneHost := seedHost(t, pool, projectID, "lone-"+randSlug(t))
	var loneInc int64
	mustScan(t, pool, &loneInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, resolved_at)
		VALUES ($1,$2,'memory','resolved',0,0,'', now()) RETURNING id`, projectID, loneHost)

	since := time.Now().Add(-time.Hour)
	items, err := store.ClosedSince(ctx, projectID, since, 50)
	if err != nil {
		t.Fatalf("ClosedSince: %v", err)
	}
	var haveLone, haveGrouped bool
	for _, it := range items {
		if it.Source == "host" && it.IncidentID == loneInc {
			haveLone = true
		}
		if it.Source == "host" && it.IncidentID == groupedInc {
			haveGrouped = true
		}
	}
	if !haveLone {
		t.Fatalf("ClosedSince must contain lone closed incident %d", loneInc)
	}
	if haveGrouped {
		t.Fatalf("ClosedSince must NOT contain grouped closed member %d", groupedInc)
	}

	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET status='resolved', resolved_at=now() WHERE id=$1`, rootInc); err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	if _, err := store.Resolve(ctx, "host", rootInc); err != nil {
		t.Fatalf("Resolve group: %v", err)
	}
	items, err = store.ClosedSince(ctx, projectID, since, 50)
	if err != nil {
		t.Fatalf("ClosedSince after root resolve: %v", err)
	}
	var haveRoot bool
	for _, it := range items {
		if it.Source == "host" && it.IncidentID == rootInc {
			haveRoot = true
		}
	}
	if !haveRoot {
		t.Fatalf("ClosedSince must contain closed root incident %d once its group is resolved (R4 fix)", rootInc)
	}
}

func TestFeedClosedRootSurvivesCardLimitEviction(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)
	since := time.Now().Add(-time.Hour)

	closeRootGroup := func(label string) int64 {
		host := seedHost(t, pool, projectID, label+"-"+randSlug(t))
		rootInc := seedSilent(t, pool, projectID, host, true)
		if _, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", host); err != nil {
			t.Fatalf("EnsureGroup %s: %v", label, err)
		}
		mustExec(t, pool, `UPDATE host_incidents SET status='resolved', resolved_at=now() WHERE id=$1`, rootInc)
		if _, err := store.Resolve(ctx, "host", rootInc); err != nil {
			t.Fatalf("Resolve %s: %v", label, err)
		}
		return rootInc
	}

	olderRoot := closeRootGroup("evicted")
	newerRoot := closeRootGroup("kept")

	cardGroups, err := store.ClosedGroupsSince(ctx, projectID, since, 1)
	if err != nil {
		t.Fatalf("ClosedGroupsSince: %v", err)
	}
	if len(cardGroups) != 1 || cardGroups[0].RootIncidentID != newerRoot {
		t.Fatalf("ClosedGroupsSince(limit=1) = %+v, want exactly the newer group (root %d)", cardGroups, newerRoot)
	}

	feedItems, err := store.ClosedSince(ctx, projectID, since, 50)
	if err != nil {
		t.Fatalf("ClosedSince: %v", err)
	}
	haveOlder, haveNewer := false, false
	for _, it := range feedItems {
		if it.Source != "host" {
			continue
		}
		if it.IncidentID == olderRoot {
			haveOlder = true
		}
		if it.IncidentID == newerRoot {
			haveNewer = true
		}
	}
	if !haveOlder {
		t.Fatalf("ClosedSince must contain the root %d whose group card was evicted by ClosedGroupsSince's limit", olderRoot)
	}
	if !haveNewer {
		t.Fatalf("ClosedSince must contain the root %d of the group whose card WAS rendered too (intentional dup, see ClosedSince docblock)", newerRoot)
	}
}

func TestFeedGroupsRootNameAndResolvedFilter(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHostName := "root-" + randSlug(t)
	rootHost := seedHost(t, pool, projectID, rootHostName)
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	gOpen, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup open: %v", err)
	}

	rootHostName2 := "root2-" + randSlug(t)
	rootHost2 := seedHost(t, pool, projectID, rootHostName2)
	rootInc2 := seedSilent(t, pool, projectID, rootHost2, true)
	gClosed, err := store.EnsureGroup(ctx, projectID, "host", rootInc2, "host", rootHost2)
	if err != nil {
		t.Fatalf("EnsureGroup closed: %v", err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc2); err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	open, err := store.OpenGroups(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenGroups: %v", err)
	}
	if len(open) != 1 || open[0].ID != gOpen.ID {
		t.Fatalf("OpenGroups = %+v, want exactly [%d]", open, gOpen.ID)
	}
	if open[0].RootName != rootHostName {
		t.Fatalf("OpenGroups RootName = %q, want %q", open[0].RootName, rootHostName)
	}

	since := time.Now().Add(-time.Hour)
	closed, err := store.ClosedGroupsSince(ctx, projectID, since, 50)
	if err != nil {
		t.Fatalf("ClosedGroupsSince: %v", err)
	}
	if len(closed) != 1 || closed[0].ID != gClosed.ID {
		t.Fatalf("ClosedGroupsSince = %+v, want exactly [%d]", closed, gClosed.ID)
	}
	if closed[0].RootName != rootHostName2 {
		t.Fatalf("ClosedGroupsSince RootName = %q, want %q", closed[0].RootName, rootHostName2)
	}
}

func TestFeedGroupRowRootSeverity(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	hostRoot := seedHost(t, pool, projectID, "root-"+randSlug(t))
	var hostRootInc int64
	mustScan(t, pool, &hostRootInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open, severity)
		VALUES ($1,$2,'silent','open',0,0,'',true,'warning') RETURNING id`, projectID, hostRoot)
	gHost, err := store.EnsureGroup(ctx, projectID, "host", hostRootInc, "host", hostRoot)
	if err != nil {
		t.Fatalf("EnsureGroup host: %v", err)
	}

	var monitorID int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	var uptimeRootInc int64
	mustScan(t, pool, &uptimeRootInc, `
		INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)
	gUptime, err := store.EnsureGroup(ctx, projectID, "uptime", uptimeRootInc, "monitor", monitorID)
	if err != nil {
		t.Fatalf("EnsureGroup uptime: %v", err)
	}

	open, err := store.OpenGroups(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenGroups: %v", err)
	}
	byID := map[int64]incidentgroup.GroupRow{}
	for _, g := range open {
		byID[g.ID] = g
	}
	gotHost, ok := byID[gHost.ID]
	if !ok {
		t.Fatalf("OpenGroups missing host-rooted group %d: %+v", gHost.ID, open)
	}
	if gotHost.RootSeverity != "warning" {
		t.Fatalf("host-rooted group RootSeverity = %q, want %q", gotHost.RootSeverity, "warning")
	}
	gotUptime, ok := byID[gUptime.ID]
	if !ok {
		t.Fatalf("OpenGroups missing uptime-rooted group %d: %+v", gUptime.ID, open)
	}
	if gotUptime.RootSeverity != "" {
		t.Fatalf("uptime-rooted group RootSeverity = %q, want empty (incidents table has no severity)", gotUptime.RootSeverity)
	}
}

func assertNoFeedLeak(t *testing.T, label string, items []incidentgroup.FeedItem, leaks map[string]int64) {
	t.Helper()
	for _, it := range items {
		if want, ok := leaks[it.Source]; ok && it.IncidentID == want {
			t.Errorf("%s: утечка чужого проекта — %s инцидент %d виден в выдаче", label, it.Source, it.IncidentID)
		}
	}
}

func TestFeedTenantIsolationSources(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	otherProjectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	fHost := seedHost(t, pool, otherProjectID, "f-host-"+randSlug(t))
	var fHostOpen, fHostClosed int64
	mustScan(t, pool, &fHostOpen, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, otherProjectID, fHost)
	mustScan(t, pool, &fHostClosed, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, resolved_at)
		VALUES ($1,$2,'memory','resolved',0,0,'', now()) RETURNING id`, otherProjectID, fHost)

	var fMonitorID, fUptimeOpen, fUptimeClosed int64
	mustScan(t, pool, &fMonitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'f-mon-`+randSlug(t)+`','http',60) RETURNING id`, otherProjectID)
	mustScan(t, pool, &fUptimeOpen, `
		INSERT INTO incidents (monitor_id, notified_open)
		VALUES ($1,true) RETURNING id`, fMonitorID)
	mustScan(t, pool, &fUptimeClosed, `
		INSERT INTO incidents (monitor_id, notified_open, resolved_at)
		VALUES ($1,true,now()) RETURNING id`, fMonitorID)

	var fRuleID, fMetricOpen, fMetricClosed int64
	mustScan(t, pool, &fRuleID, `
		INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		VALUES ($1,'f.cpu.load','avg','gt',0.9) RETURNING id`, otherProjectID)
	mustScan(t, pool, &fMetricOpen, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1,$2,1,1) RETURNING id`, fRuleID, otherProjectID)
	mustScan(t, pool, &fMetricClosed, `
		INSERT INTO metric_incidents (rule_id, project_id, status, peak_value, current_value, resolved_at)
		VALUES ($1,$2,'resolved',1,1,now()) RETURNING id`, fRuleID, otherProjectID)

	var fSloID, fSloOpen, fSloClosed int64
	mustScan(t, pool, &fSloID, `
		INSERT INTO slos (project_id, name, sli_kind, target, window_days)
		VALUES ($1,'f-slo-`+randSlug(t)+`','availability',0.99,30) RETURNING id`, otherProjectID)
	mustScan(t, pool, &fSloOpen, `
		INSERT INTO slo_incidents (slo_id, project_id, burn_rate)
		VALUES ($1,$2,20) RETURNING id`, fSloID, otherProjectID)
	mustScan(t, pool, &fSloClosed, `
		INSERT INTO slo_incidents (slo_id, project_id, status, burn_rate, resolved_at)
		VALUES ($1,$2,'resolved',20,now()) RETURNING id`, fSloID, otherProjectID)

	var fTraceOpen, fTraceClosed int64
	mustScan(t, pool, &fTraceOpen, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1,'endpoint_p95','/api/f-open','duration',100,500,500) RETURNING id`, otherProjectID)
	mustScan(t, pool, &fTraceClosed, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, status, baseline_value, peak_value, current_value, resolved_at)
		VALUES ($1,'endpoint_p95','/api/f-closed','duration','resolved',100,500,500,now()) RETURNING id`, otherProjectID)

	var fProfileOpen, fProfileClosed int64
	mustScan(t, pool, &fProfileOpen, `
		INSERT INTO profile_regressions (project_id, service, profile_type, function, baseline_share, peak_share, current_share)
		VALUES ($1,'svc','cpu','doFOpen',0.1,0.5,0.5) RETURNING id`, otherProjectID)
	mustScan(t, pool, &fProfileClosed, `
		INSERT INTO profile_regressions (project_id, service, profile_type, function, status, baseline_share, peak_share, current_share, resolved_at)
		VALUES ($1,'svc','cpu','doFClosed','resolved',0.1,0.5,0.5,now()) RETURNING id`, otherProjectID)

	openLeaks := map[string]int64{
		"host": fHostOpen, "uptime": fUptimeOpen, "metric": fMetricOpen,
		"slo": fSloOpen, "trace": fTraceOpen, "profile": fProfileOpen,
	}
	closedLeaks := map[string]int64{
		"host": fHostClosed, "uptime": fUptimeClosed, "metric": fMetricClosed,
		"slo": fSloClosed, "trace": fTraceClosed, "profile": fProfileClosed,
	}

	open, err := store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup: %v", err)
	}
	assertNoFeedLeak(t, "OpenOutOfGroup", open, openLeaks)

	since := time.Now().Add(-time.Hour)
	closed, err := store.ClosedSince(ctx, projectID, since, 50)
	if err != nil {
		t.Fatalf("ClosedSince: %v", err)
	}
	assertNoFeedLeak(t, "ClosedSince", closed, closedLeaks)
}

func TestFeedTenantIsolationGroups(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	otherProjectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	fOpenHost := seedHost(t, pool, otherProjectID, "f-open-"+randSlug(t))
	fOpenInc := seedSilent(t, pool, otherProjectID, fOpenHost, true)
	fOpenGroup, err := store.EnsureGroup(ctx, otherProjectID, "host", fOpenInc, "host", fOpenHost)
	if err != nil {
		t.Fatalf("EnsureGroup open (чужой проект): %v", err)
	}

	fClosedHost := seedHost(t, pool, otherProjectID, "f-closed-"+randSlug(t))
	fClosedInc := seedSilent(t, pool, otherProjectID, fClosedHost, true)
	fClosedGroup, err := store.EnsureGroup(ctx, otherProjectID, "host", fClosedInc, "host", fClosedHost)
	if err != nil {
		t.Fatalf("EnsureGroup closed (чужой проект): %v", err)
	}
	if ok, err := store.Resolve(ctx, "host", fClosedInc); err != nil || !ok {
		t.Fatalf("Resolve (чужой проект): ok=%v err=%v", ok, err)
	}

	open, err := store.OpenGroups(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenGroups: %v", err)
	}
	for _, g := range open {
		if g.ID == fOpenGroup.ID {
			t.Errorf("OpenGroups: утечка чужой открытой группы %d", fOpenGroup.ID)
		}
	}

	since := time.Now().Add(-time.Hour)
	closed, err := store.ClosedGroupsSince(ctx, projectID, since, 50)
	if err != nil {
		t.Fatalf("ClosedGroupsSince: %v", err)
	}
	for _, g := range closed {
		if g.ID == fClosedGroup.ID {
			t.Errorf("ClosedGroupsSince: утечка чужой закрытой группы %d", fClosedGroup.ID)
		}
	}
}

func TestFeedOpenMemberOfResolvedGroupVisible(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHostName := "root-" + randSlug(t)
	rootHost := seedHost(t, pool, projectID, rootHostName)
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
	if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup: ok=%v err=%v", ok, err)
	}

	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	items, err := store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup: %v", err)
	}
	var found *incidentgroup.FeedItem
	for i := range items {
		if items[i].Source == "host" && items[i].IncidentID == memberInc {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatalf("open member of a resolved group must appear in OpenOutOfGroup: %+v", items)
	}
	if found.FormerGroupID != g.ID {
		t.Fatalf("FormerGroupID = %d, want %d", found.FormerGroupID, g.ID)
	}
	if found.FormerGroupRootName != rootHostName {
		t.Fatalf("FormerGroupRootName = %q, want %q", found.FormerGroupRootName, rootHostName)
	}

	members, err := store.Composition(ctx, projectID, g.ID)
	if err != nil {
		t.Fatalf("Composition: %v", err)
	}
	var stillMember bool
	for _, m := range members {
		if m.Source == "host" && m.IncidentID == memberInc {
			stillMember = true
		}
	}
	if !stillMember {
		t.Fatalf("member must remain in the closed group's composition: %+v", members)
	}
}

func TestFeedMemberOfPurgedGroupVisible(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
	if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup: ok=%v err=%v", ok, err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM incident_groups WHERE id = $1`, g.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}

	items, err := store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup: %v", err)
	}
	var found *incidentgroup.FeedItem
	for i := range items {
		if items[i].Source == "host" && items[i].IncidentID == memberInc {
			found = &items[i]
		}
	}
	if found == nil {
		t.Fatalf("member of a purged group must appear in OpenOutOfGroup: %+v", items)
	}
	if found.FormerGroupID != 0 {
		t.Fatalf("FormerGroupID of a purged group must be 0, got %d", found.FormerGroupID)
	}
}

func TestFeedClosedMemberOfResolvedGroupVisible(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, resolved_at)
		VALUES ($1,$2,'disk','resolved',0,0,'', now()) RETURNING id`, projectID, memberHost)
	if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup: ok=%v err=%v", ok, err)
	}

	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	items, err := store.ClosedSince(ctx, projectID, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("ClosedSince: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.Source == "host" && it.IncidentID == memberInc {
			found = true
		}
	}
	if !found {
		t.Fatalf("closed member of a now-resolved group must appear in ClosedSince: %+v", items)
	}
}

func TestFeedOpenGroupsLimit(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	for i := 0; i < incidentgroup.MaxOpenGroups+1; i++ {
		host := seedHost(t, pool, projectID, "og-"+randSlug(t))
		rootInc := seedSilent(t, pool, projectID, host, true)
		if _, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", host); err != nil {
			t.Fatalf("EnsureGroup #%d: %v", i, err)
		}
	}

	open, err := store.OpenGroups(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenGroups: %v", err)
	}
	if len(open) != incidentgroup.MaxOpenGroups {
		t.Fatalf("OpenGroups len = %d, want exactly MaxOpenGroups (%d) even though %d groups exist",
			len(open), incidentgroup.MaxOpenGroups, incidentgroup.MaxOpenGroups+1)
	}
}

func TestFeedOpenOutOfGroupLimit(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	for i := 0; i < incidentgroup.MaxOpenOutOfGroup+1; i++ {
		mustExec(t, pool, `
			INSERT INTO perf_regressions (project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
			VALUES ($1,'endpoint_p95',$2,'duration',100,500,500)`, projectID, "/api/"+randSlug(t))
	}

	items, err := store.OpenOutOfGroup(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenOutOfGroup: %v", err)
	}
	if len(items) != incidentgroup.MaxOpenOutOfGroup {
		t.Fatalf("OpenOutOfGroup len = %d, want exactly MaxOpenOutOfGroup (%d) even though %d items exist",
			len(items), incidentgroup.MaxOpenOutOfGroup, incidentgroup.MaxOpenOutOfGroup+1)
	}
}

func TestFeedCompositionsBatch(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	hostA := seedHost(t, pool, projectID, "a-"+randSlug(t))
	rootA := seedSilent(t, pool, projectID, hostA, false)
	gA, err := store.EnsureGroup(ctx, projectID, "host", rootA, "host", hostA)
	if err != nil {
		t.Fatalf("EnsureGroup A: %v", err)
	}
	memberHostA := seedHost(t, pool, projectID, "a-member-"+randSlug(t))
	memberA := seedFeedHostIncidentOpen(t, pool, projectID, memberHostA, "disk")
	if _, err := store.SetGroup(ctx, projectID, "host", memberA, gA.ID); err != nil {
		t.Fatalf("SetGroup A: %v", err)
	}

	hostB := seedHost(t, pool, projectID, "b-"+randSlug(t))
	rootB := seedSilent(t, pool, projectID, hostB, true)
	gB, err := store.EnsureGroup(ctx, projectID, "host", rootB, "host", hostB)
	if err != nil {
		t.Fatalf("EnsureGroup B: %v", err)
	}
	memberHostB := seedHost(t, pool, projectID, "b-member-"+randSlug(t))
	memberB := seedFeedHostIncidentOpen(t, pool, projectID, memberHostB, "memory")
	if _, err := store.SetGroup(ctx, projectID, "host", memberB, gB.ID); err != nil {
		t.Fatalf("SetGroup B: %v", err)
	}

	got, err := store.Compositions(ctx, projectID, []int64{gA.ID, gB.ID})
	if err != nil {
		t.Fatalf("Compositions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Compositions map len = %d, want 2 groups", len(got))
	}
	if len(got[gA.ID]) != 1 || got[gA.ID][0].IncidentID != memberA {
		t.Fatalf("Compositions[gA] = %+v, want exactly member %d", got[gA.ID], memberA)
	}
	if len(got[gB.ID]) != 1 || got[gB.ID][0].IncidentID != memberB {
		t.Fatalf("Compositions[gB] = %+v, want exactly member %d", got[gB.ID], memberB)
	}
	if got[gA.ID][0].HeldByGroup {
		t.Fatalf("member of A (non-informing root) must have HeldByGroup=false, got true")
	}
	if !got[gB.ID][0].HeldByGroup {
		t.Fatalf("member of B (informing root) must have HeldByGroup=true, got false")
	}

	wantA, err := store.Composition(ctx, projectID, gA.ID)
	if err != nil {
		t.Fatalf("Composition gA: %v", err)
	}
	if len(wantA) != len(got[gA.ID]) || wantA[0].IncidentID != got[gA.ID][0].IncidentID {
		t.Fatalf("Compositions[gA] = %+v, Composition(gA) = %+v, must match", got[gA.ID], wantA)
	}
}

func TestFeedCompositionsEmptyGroupIDs(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	got, err := store.Compositions(ctx, projectID, nil)
	if err != nil {
		t.Fatalf("Compositions(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Compositions(nil) = %+v, want empty map", got)
	}
}

func TestFeedCompositionsTenantIsolation(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	otherProjectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	host := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, host, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", host)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	memberHost := seedHost(t, pool, projectID, "member-"+randSlug(t))
	memberInc := seedFeedHostIncidentOpen(t, pool, projectID, memberHost, "disk")
	if _, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	got, err := store.Compositions(ctx, otherProjectID, []int64{g.ID})
	if err != nil {
		t.Fatalf("Compositions(otherProject): %v", err)
	}
	if len(got[g.ID]) != 0 {
		t.Fatalf("Compositions under the wrong project must not leak the group's members: %+v", got)
	}
}

func seedFeedHostIncidentOpen(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, kind string) int64 {
	t.Helper()
	var id int64
	mustScan(t, pool, &id, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,$3,'open',0,0,'') RETURNING id`, projectID, hostID, kind)
	return id
}
