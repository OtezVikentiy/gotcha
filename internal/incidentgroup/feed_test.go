package incidentgroup_test

import (
	"context"
	"testing"
	"time"

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

	// host-член.
	memberHostName := "hostmember-" + randSlug(t)
	memberHost := seedHost(t, pool, projectID, memberHostName)
	var hostMemberInc int64
	mustScan(t, pool, &hostMemberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
	if err := store.SetGroup(ctx, "host", hostMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup host: %v", err)
	}

	// uptime-член, подавлен зависимостью.
	var monitorID, uptimeMemberInc int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &uptimeMemberInc, `
		INSERT INTO incidents (monitor_id, notified_open, suppressed_by_dep)
		VALUES ($1,true,true) RETURNING id`, monitorID)
	if err := store.SetGroup(ctx, "uptime", uptimeMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup uptime: %v", err)
	}

	// metric-член.
	var ruleID, metricMemberInc int64
	mustScan(t, pool, &ruleID, `
		INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		VALUES ($1,'cpu.load','avg','gt',0.9) RETURNING id`, projectID)
	mustScan(t, pool, &metricMemberInc, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1,$2,1,1) RETURNING id`, ruleID, projectID)
	if err := store.SetGroup(ctx, "metric", metricMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup metric: %v", err)
	}

	// slo-член.
	var sloID, sloMemberInc int64
	mustScan(t, pool, &sloID, `
		INSERT INTO slos (project_id, name, sli_kind, target, window_days)
		VALUES ($1,'slo-`+randSlug(t)+`','availability',0.99,30) RETURNING id`, projectID)
	mustScan(t, pool, &sloMemberInc, `
		INSERT INTO slo_incidents (slo_id, project_id, burn_rate)
		VALUES ($1,$2,20) RETURNING id`, sloID, projectID)
	if err := store.SetGroup(ctx, "slo", sloMemberInc, g.ID); err != nil {
		t.Fatalf("SetGroup slo: %v", err)
	}

	members, err := store.Composition(ctx, g.ID)
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

	u, ok := bySource["uptime"]
	if !ok {
		t.Fatalf("Composition missing uptime member")
	}
	if u.IncidentID != uptimeMemberInc || !u.SuppressedByDep {
		t.Fatalf("uptime member: IncidentID=%d SuppressedByDep=%v, want %d/true", u.IncidentID, u.SuppressedByDep, uptimeMemberInc)
	}

	mm, ok := bySource["metric"]
	if !ok {
		t.Fatalf("Composition missing metric member")
	}
	if mm.IncidentID != metricMemberInc || mm.Title != "cpu.load" {
		t.Fatalf("metric member: IncidentID=%d Title=%q, want %d/cpu.load", mm.IncidentID, mm.Title, metricMemberInc)
	}

	s, ok := bySource["slo"]
	if !ok {
		t.Fatalf("Composition missing slo member")
	}
	if s.IncidentID != sloMemberInc {
		t.Fatalf("slo member: IncidentID=%d, want %d", s.IncidentID, sloMemberInc)
	}
}

func TestFeedOpenOutOfGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	// Группа с членом — не должна попасть в OpenOutOfGroup.
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
	if err := store.SetGroup(ctx, "host", groupedInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Внегрупповой открытый host-инцидент.
	loneHost := seedHost(t, pool, projectID, "lone-"+randSlug(t))
	var loneInc int64
	mustScan(t, pool, &loneInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'memory','open',0,0,'') RETURNING id`, projectID, loneHost)

	// Открытая trace-регрессия — всегда вне групп.
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
	// Корень группы сам никогда не SetGroup'ится (только члены — см.
	// Grouper.Attach): его собственная host_incidents-строка остаётся с
	// group_id NULL и продолжает жить как обычный открытый инцидент (та же
	// логика, что и видимость корня в host.OpenUnacked, см. janitor_test.go
	// TestSweepClosesGroupWhenRootDeleted — прячутся только члены).
	if !haveRoot {
		t.Fatalf("OpenOutOfGroup must contain the group's own root incident %d (root is never SetGroup'd)", rootInc)
	}
}

func TestFeedClosedSince(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	// Группа с закрытым членом — член не должен попасть в ClosedSince
	// (показан внутри свёрнутой карточки группы, не дублируется).
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
	if err := store.SetGroup(ctx, "host", groupedInc, g.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	// Внегрупповой закрытый host-инцидент — должен попасть.
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
