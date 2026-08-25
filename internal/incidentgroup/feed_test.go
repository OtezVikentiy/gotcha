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
	if _, err := store.SetGroup(ctx, projectID, "host", hostMemberInc, g.ID); err != nil {
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
	if _, err := store.SetGroup(ctx, projectID, "uptime", uptimeMemberInc, g.ID); err != nil {
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
	if _, err := store.SetGroup(ctx, projectID, "metric", metricMemberInc, g.ID); err != nil {
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
	if _, err := store.SetGroup(ctx, projectID, "host", groupedInc, g.ID); err != nil {
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
	// Grouper.Attach), но во «Вне групп» его быть не должно: он уже показан
	// в шапке карточки своей группы, иначе один инцидент виден дважды.
	if haveRoot {
		t.Fatalf("OpenOutOfGroup must NOT contain the group's own root incident %d (shown in the group card header)", rootInc)
	}

	// Осиротевшая группа удалена janitor'ом — карточки больше нет, и корень
	// обязан вернуться во «Вне групп» обычной строкой.
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

// TestFeedRootNotDuplicatedUptime — тот же запрет дубля для uptime-корня
// (вторая ветка условия) и для закрытой ленты: корень закрытой группы виден
// в свёрнутой карточке, во «Вне групп» его нет.
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

	// Закрываем корень и группу — корень не должен всплыть в ClosedSince.
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
	for _, it := range closed {
		if it.Source == "uptime" && it.IncidentID == rootInc {
			t.Fatalf("ClosedSince must NOT contain uptime root incident %d", rootInc)
		}
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
	if _, err := store.SetGroup(ctx, projectID, "host", groupedInc, g.ID); err != nil {
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

	// Закрытый корень группы тоже не дублируется: он в шапке свёрнутой
	// карточки, отдельной строкой в «закрытых» его быть не должно.
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
	for _, it := range items {
		if it.Source == "host" && it.IncidentID == rootInc {
			t.Fatalf("ClosedSince must NOT contain closed root incident %d", rootInc)
		}
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

// assertNoFeedLeak — ни один FeedItem из items не должен совпадать по
// Source+IncidentID со значением из leaks (индексировано по Source):
// сторож против того, что project_id-фильтр одной из веток feedProjectQuery
// снят (см. TestFeedTenantIsolationSources).
func assertNoFeedLeak(t *testing.T, label string, items []incidentgroup.FeedItem, leaks map[string]int64) {
	t.Helper()
	for _, it := range items {
		if want, ok := leaks[it.Source]; ok && it.IncidentID == want {
			t.Errorf("%s: утечка чужого проекта — %s инцидент %d виден в выдаче", label, it.Source, it.IncidentID)
		}
	}
}

// TestFeedTenantIsolationSources — OpenOutOfGroup/ClosedSince объединяют 6
// источников через общую feedProjectQuery (host/uptime/metric/slo/trace/
// profile), каждый со своей веткой WHERE ...project_id = $1 AND <cond>.
// Тест сидирует по одному ОТКРЫТОМУ и одному ЗАКРЫТОМУ инциденту КАЖДОГО из
// 6 источников в ЧУЖОМ проекте и проверяет, что ни один не просачивается в
// выдачу для другого (своего) projectID — снятие тенант-фильтра в любой из
// 6 веток (например trace/profile, у которых project_id хранится прямо на
// таблице, а не через JOIN, как у host/metric/slo/uptime) должно быть
// поймано именно здесь, а не только на host-ветке, которую уже покрывали
// TestFeedOpenOutOfGroup/TestFeedClosedSince.
func TestFeedTenantIsolationSources(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	otherProjectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	// host: чужой открытый + закрытый инцидент.
	fHost := seedHost(t, pool, otherProjectID, "f-host-"+randSlug(t))
	var fHostOpen, fHostClosed int64
	mustScan(t, pool, &fHostOpen, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, otherProjectID, fHost)
	mustScan(t, pool, &fHostClosed, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, resolved_at)
		VALUES ($1,$2,'memory','resolved',0,0,'', now()) RETURNING id`, otherProjectID, fHost)

	// uptime: чужой монитор, открытый + закрытый инцидент.
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

	// metric: чужое правило, открытый + закрытый инцидент.
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

	// slo: чужой SLO, открытый + закрытый инцидент.
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

	// trace (perf_regressions): открытый + закрытый, project_id прямо на таблице.
	var fTraceOpen, fTraceClosed int64
	mustScan(t, pool, &fTraceOpen, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, baseline_value, peak_value, current_value)
		VALUES ($1,'endpoint_p95','/api/f-open','duration',100,500,500) RETURNING id`, otherProjectID)
	mustScan(t, pool, &fTraceClosed, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, status, baseline_value, peak_value, current_value, resolved_at)
		VALUES ($1,'endpoint_p95','/api/f-closed','duration','resolved',100,500,500,now()) RETURNING id`, otherProjectID)

	// profile (profile_regressions): открытый + закрытый, project_id прямо на таблице.
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

// TestFeedTenantIsolationGroups — OpenGroups/ClosedGroupsSince фильтруют
// incident_groups по одной колонке (g.project_id = $1), но покрытия на
// два проекта не было ни у одного теста групп — закрываем тем же приёмом,
// что и TestFeedTenantIsolationSources: чужая открытая и чужая закрытая
// группа не должны попасть в выдачу для другого projectID.
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

// TestFeedOpenMemberOfResolvedGroupVisible — W1: открытый член группы, чей
// корень уже закрылся (группа резолвнута), обязан появиться во «Вне групп»
// СРАЗУ, одновременно оставаясь в составе свёрнутой карточки закрытой
// группы (Composition) — два разных смысла («открытая работа» vs «упало
// вместе с этим»), не тот дубль, что чинили для корня. FormerGroup* несёт
// данные для бейджа feed.badge.was_grouped (бейдж рисует R5).
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

// TestFeedMemberOfPurgedGroupVisible — W1: то же самое, но группа не
// резолвнута, а УДАЛЕНА (janitor purge) — group_id члена висит на
// несуществующую строку. Трактовка та же: член вне группы. FormerGroupID=0
// — сведений о бывшей группе нигде не осталось (строка удалена).
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

// TestFeedClosedMemberOfResolvedGroupVisible — W1, симметрия с
// OpenOutOfGroup: закрывшийся член ЗАКРЫТОЙ группы попадает в ClosedSince —
// «член резолвнутой группы = вне группы» применяется одинаково к открытым
// и закрытым членам, не только к открытым.
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
