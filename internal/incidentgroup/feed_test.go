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
	// Корень (seedSilent ... true) информирующий, группа и сам член
	// открыты — все 4 источника обязаны нести HeldByGroup=true (W15): это
	// не то же самое, что SuppressedByDep (B5, независимый механизм) — тут
	// корень как раз НЕ suppressed_by_dep, но host-член всё равно молчит,
	// потому что уведомил корень.
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
	// SuppressedByDep и HeldByGroup — независимые механизмы (B5 vs D3
	// groupGate): у uptime-члена здесь оба true одновременно, каждый по
	// своей причине, ни один не подменяет другой.
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
	// metric_incidents не несёт своей колонки suppressed_by_dep (B5
	// неприменим), но HeldByGroup обязан считаться так же, как для host —
	// это и есть W15: до фикса он был захардкожен в false для metric/slo.
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

// TestFeedCompositionHeldByGroupGates — W15, три независимых гейта,
// каждый способен в одиночку выключить HeldByGroup, несмотря на то, что
// остальные условия выполнены: немой корень (notified_open=false), сам член
// уже закрылся, группа уже закрыта (корень резолвнут). Каждый подтест валит
// РОВНО одно условие — так мутация, вырезавшая одно `AND` в CTE grp
// (feedMemberSelect), ловится конкретным подтестом, а не тонет в остальных.
func TestFeedCompositionHeldByGroupGates(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	store := incidentgroup.NewStore(pool)

	t.Run("silent root", func(t *testing.T) {
		projectID := seedProject(t, pool)
		rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
		rootInc := seedSilent(t, pool, projectID, rootHost, false) // notified_open=false — немой корень
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
// (вторая ветка условия): пока группа открыта, корень во «Вне групп» не
// показывается (он в шапке карточки открытой группы). Как только группа
// закрывается, корень должен появиться в ClosedSince (R4, W7-warning) — это
// уже не дубль с (возможно не отрисованной) карточкой закрытой группы, а
// защита от исчезновения с ленты целиком.
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

	// Закрываем корень и группу — теперь корень ОБЯЗАН всплыть в ClosedSince
	// (R4, W7-warning): раньше uptimeNotRoot исключал корень безусловно,
	// пока хоть какая-то (даже давно закрытая) группа на него ссылалась —
	// резолвнутый корень резолвнутой группы был виден только в шапке
	// свёрнутой карточки ClosedGroupsSince и пропадал с ленты целиком, если
	// сама карточка не отрисовывалась (окно/потолок). uptimeNotRoot теперь
	// исключает корень только пока его группа ЕЩЁ открыта (см. докблок
	// hostNotRoot/uptimeNotRoot) — симметрично тому, как notOpenGroupMember
	// уже трактует обычных членов.
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

	// Закрытый корень группы (R4, W7-warning): раньше hostNotRoot исключал
	// его из ClosedSince безусловно, пока хоть какая-то (даже давно
	// закрытая) группа на него ссылалась — он был виден ТОЛЬКО в шапке
	// свёрнутой карточки ClosedGroupsSince и пропадал с ленты целиком, если
	// сама карточка не отрисовывалась. Теперь исключение снято, как только
	// группа закрывается — root ведёт себя как обычный резолвнутый
	// инцидент источника, симметрично членам (haveGrouped/haveLone выше).
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

// TestFeedClosedRootSurvivesCardLimitEviction — сценарий, явно
// потребованный ревью R4 (W7-warning): ClosedGroupsSince и ClosedSince
// теперь получают РАЗНЫЕ потолки (W8, incidentfeed.go), и если бы
// hostNotRoot по-прежнему исключал резолвнутый корень безусловно, у группы,
// чья карточка не поместилась в потолок ClosedGroupsSince, корень пропадал
// бы с ленты насовсем — ни в (неотрисованной) карточке, ни в списке
// закрытых. Заводим 2 закрытые группы, ClosedGroupsSince(..., limit=1)
// отдаёт только САМУЮ свежую (карточка второй не отрисуется на странице) —
// и всё равно требуем, чтобы корень ОБЕИХ групп присутствовал в
// ClosedSince: лимит карточек не должен решать судьбу видимости корня.
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

	// Потолок карточек = 1: отрисуется только самая свежая группа (newerRoot).
	cardGroups, err := store.ClosedGroupsSince(ctx, projectID, since, 1)
	if err != nil {
		t.Fatalf("ClosedGroupsSince: %v", err)
	}
	if len(cardGroups) != 1 || cardGroups[0].RootIncidentID != newerRoot {
		t.Fatalf("ClosedGroupsSince(limit=1) = %+v, want exactly the newer group (root %d)", cardGroups, newerRoot)
	}

	// Лента (потолок отдельный, W8) обязана показать ОБА корня — включая
	// тот, чья карточка только что была отрезана лимитом ClosedGroupsSince.
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

// TestFeedGroupRowRootSeverity — W24: GroupRow несёт RootSeverity корневого
// инцидента (карточка ленты рисует по нему severity-бейдж, §6.1/1). Host-
// корень несёт severity host_incidents (проверяем явный 'warning', чтобы не
// спутать с молчаливым совпадением через DEFAULT 'critical'); uptime-корень
// — всегда ” (таблица `incidents` вовсе не несёт колонки severity, сама
// категория неприменима к даунтайму монитора).
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

// TestFeedOpenGroupsLimit — W7: OpenGroups больше не идёт без LIMIT. Заводим
// MaxOpenGroups+1 открытых групп в одном проекте и проверяем, что стор
// отдаёт ровно MaxOpenGroups — на доступной ЛЮБОМУ участнику странице
// (incident-feed, lvlAccess) неограниченная выборка была бы поводом для
// шторма на большом проекте.
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

// TestFeedOpenOutOfGroupLimit — W7: тот же потолок у OpenOutOfGroup.
// MaxOpenOutOfGroup+1 внегрупповых открытых perf-регрессий (самый дешёвый
// источник для сидирования — не требует host/monitor), стор обязан отдать
// не больше MaxOpenOutOfGroup строк.
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

// TestFeedCompositionsBatch — W7: Compositions (group_id = ANY($2)) обязана
// вернуть РОВНО тот же состав, что и Composition в цикле (старый путь
// incidentfeed.go, N+1) — по группе на группу, без утечки строк между
// группами через общий CTE grp (главный риск батч-версии: JOIN grp ON true
// в одиночном запросе годился только потому что там была одна строка на
// весь запрос; в батче тот же промах молча подмешал бы root_notified_open
// чужой группы всем остальным).
func TestFeedCompositionsBatch(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	// Группа A: host-корень НЕ информирующий (notified_open=false) — член
	// не должен получить HeldByGroup=true.
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

	// Группа B: host-корень информирующий (notified_open=true) — член
	// должен получить HeldByGroup=true. Если grp CTE в батче перепутает
	// группы, HeldByGroup у члена A и B тут же совпадут (оба true либо оба
	// false) — тест это ловит сравнением обеих групп в одном ответе.
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

	// Сверка с одиночным Composition — тот же состав по каждой группе.
	wantA, err := store.Composition(ctx, projectID, gA.ID)
	if err != nil {
		t.Fatalf("Composition gA: %v", err)
	}
	if len(wantA) != len(got[gA.ID]) || wantA[0].IncidentID != got[gA.ID][0].IncidentID {
		t.Fatalf("Compositions[gA] = %+v, Composition(gA) = %+v, must match", got[gA.ID], wantA)
	}
}

// TestFeedCompositionsEmptyGroupIDs — пустой список групп не должен ходить в
// БД вовсе (частый случай: свежий проект без единой группы) и не должен
// падать — просто пустая карта.
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

// TestFeedCompositionsTenantIsolation — W6: группа чужого проекта не должна
// протечь в батче, той же гарантией, что и у Composition.
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

// seedFeedHostIncidentOpen — открытый host_incidents минимального вида, для
// тестов Compositions выше (свой хелпер feedIncidentFeedStack недоступен
// отсюда — internal/web, другой пакет).
func seedFeedHostIncidentOpen(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, kind string) int64 {
	t.Helper()
	var id int64
	mustScan(t, pool, &id, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,$3,'open',0,0,'') RETURNING id`, projectID, hostID, kind)
	return id
}
