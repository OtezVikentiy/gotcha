package incidentgroup_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fixedRootResolver — RootResolver (см. grouper.go), чей DownRoot всегда
// возвращает заданный фиксированный ответ. Нужен для сценариев, которые
// настоящий depsuppress.Suppressor воспроизвести не может: неизвестный
// rootKind, гонка «корень исчез между DownRoot-снимком и запросом».
type fixedRootResolver struct {
	rootKind string
	rootID   int64
	found    bool
}

func (f *fixedRootResolver) DownRoot(ctx context.Context, kind string, nodeID int64) (string, int64, bool, error) {
	return f.rootKind, f.rootID, f.found, nil
}
func (f *fixedRootResolver) Invalidate() {}

// TestAttachUnderMonitorRoot — down-корень является монитором (uptime), а не
// хостом: rootIncident обязан зарезолвить ветку "monitor" (JOIN incidents/
// monitors), отдельную от ветки "host", и группа обязана заякориться на
// root_source='uptime'.
func TestAttachUnderMonitorRoot(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	var monitorID int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	var rootInc int64
	mustScan(t, pool, &rootInc, `
		INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)

	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	mustExec(t, pool, `
		INSERT INTO alert_dependencies (project_id, parent_monitor_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, monitorID, childHost)

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
		t.Fatalf("want attached under informing monitor root, got attached=%v informing=%v", attached, informing)
	}
	var groupID int64
	mustScan(t, pool, &groupID, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc)
	var gotSource string
	var gotRootInc int64
	if err := pool.QueryRow(ctx, `SELECT root_source, root_incident_id FROM incident_groups WHERE id = $1`, groupID).
		Scan(&gotSource, &gotRootInc); err != nil {
		t.Fatalf("read group: %v", err)
	}
	if gotSource != "uptime" || gotRootInc != rootInc {
		t.Fatalf("group must anchor to uptime root incident %d, got source=%q incident=%d", rootInc, gotSource, gotRootInc)
	}
}

// TestAttachUnknownRootKindErrors — DownRoot вернул rootKind, которого
// rootIncident не знает (защитная ветка default): Attach обязан
// прокинуть ошибку, а не молча проигнорировать/запаниковать.
func TestAttachUnknownRootKindErrors(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	g := &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: &fixedRootResolver{rootKind: "bogus", rootID: 1, found: true},
	}
	attached, _, err := g.Attach(ctx, "host", memberInc, "host", memberHost)
	if err == nil {
		t.Fatal("Attach with unknown root kind must return an error, got nil")
	}
	if attached {
		t.Fatal("Attach must not report attached on error")
	}
}

// TestAttachRootIncidentVanished — DownRoot нашёл корень (found=true), но у
// него уже нет открытой строки инцидента (гонка: корень закрылся между
// снимком DownRoot и этим запросом). rootIncident обязан вернуть
// (…, found=false, nil) — не ошибку — и Attach обязан тихо отказаться от
// присоединения, ведя себя как «без группы».
func TestAttachRootIncidentVanished(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	// rootHost существует, но у него НЕТ открытого silent-инцидента — то
	// есть DownRoot формально указывает на него, а rootIncident его не найдёт.
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	g := &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: &fixedRootResolver{rootKind: "host", rootID: rootHost, found: true},
	}
	attached, informing, err := g.Attach(ctx, "host", memberInc, "host", memberHost)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if attached || informing {
		t.Fatalf("vanished root incident must not attach: attached=%v informing=%v", attached, informing)
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatal("member must remain ungrouped when the root incident has vanished")
	}
}

// TestAttachSkipsAlreadyResolvedGroup — группа корня уже закрыта (Resolve
// прошёл раньше, до попытки нового присоединения — гонка sweep/attach):
// Attach обязан вести себя как «без группы» и не воскрешать закрытую
// группу новым членом.
func TestAttachSkipsAlreadyResolvedGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	g := &incidentgroup.Grouper{
		Pool:  pool,
		Store: store,
		Roots: &fixedRootResolver{rootKind: "host", rootID: rootHost, found: true},
	}
	attached, _, err := g.Attach(ctx, "host", memberInc, "host", memberHost)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if attached {
		t.Fatal("Attach must not attach a member to an already-resolved group")
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatalf("member must not be attached to resolved group %d", grp.ID)
	}
}

// TestAttachUnknownSourceErrors — SetGroup отвергает неизвестный source
// (см. group.go sourceTables); Attach обязан прокинуть эту ошибку, а не
// молчаливо считать член присоединённым.
func TestAttachUnknownSourceErrors(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	seedSilent(t, pool, projectID, rootHost, true)

	g, _ := newGrouper(pool)
	attached, _, err := g.Attach(ctx, "bogus-source", 999999, "host", rootHost)
	if err == nil {
		t.Fatal("Attach with unknown member source must return an error")
	}
	if attached {
		t.Fatal("Attach must not report attached when SetGroup errors")
	}
}

// TestAttachMetricByHostLabel — AttachMetric резолвит хост по имени в
// пределах проекта и присоединяет metric-инцидент к группе его down-корня
// (Р1: правило с label_key='host'). Хост из сценария сам является открытым
// down-корнем (silent), поэтому ожидаем присоединение к его же группе.
func TestAttachMetricByHostLabel(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostName := "gw-" + randSlug(t)
	hostID := seedHost(t, pool, projectID, hostName)
	rootInc := seedSilent(t, pool, projectID, hostID, true)

	var ruleID, metricInc int64
	mustScan(t, pool, &ruleID, `
		INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold, label_key, label_value)
		VALUES ($1,'cpu.load','avg','gt',0.9,'host',$2) RETURNING id`, projectID, hostName)
	mustScan(t, pool, &metricInc, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1,$2,1,1) RETURNING id`, ruleID, projectID)

	g, _ := newGrouper(pool)
	attached, informing, err := g.AttachMetric(ctx, metricInc, projectID, hostName)
	if err != nil {
		t.Fatalf("AttachMetric: %v", err)
	}
	if !attached || !informing {
		t.Fatalf("want attached/informing = true/true, got %v/%v", attached, informing)
	}
	var groupID int64
	mustScan(t, pool, &groupID, `SELECT group_id FROM metric_incidents WHERE id = $1`, metricInc)
	var gotRootInc int64
	mustScan(t, pool, &gotRootInc, `SELECT root_incident_id FROM incident_groups WHERE id = $1`, groupID)
	if gotRootInc != rootInc {
		t.Fatalf("group must anchor to root incident %d, got %d", rootInc, gotRootInc)
	}
}

// TestAttachMetricUnknownHostName — метка label_value не указывает ни на
// один хост проекта (переименован/удалён/опечатка) — AttachMetric обязан
// тихо вернуть attached=false, nil (не ошибку).
func TestAttachMetricUnknownHostName(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)

	var ruleID, metricInc int64
	mustScan(t, pool, &ruleID, `
		INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold, label_key, label_value)
		VALUES ($1,'cpu.load','avg','gt',0.9,'host','does-not-exist') RETURNING id`, projectID)
	mustScan(t, pool, &metricInc, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1,$2,1,1) RETURNING id`, ruleID, projectID)

	g, _ := newGrouper(pool)
	attached, _, err := g.AttachMetric(ctx, metricInc, projectID, "does-not-exist")
	if err != nil {
		t.Fatalf("AttachMetric: %v", err)
	}
	if attached {
		t.Fatal("AttachMetric must not attach when the label doesn't resolve to a known host")
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM metric_incidents WHERE id = $1`, metricInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatal("metric incident must remain ungrouped")
	}
}

// TestOnRootOpenedSkipsNonMatchingCandidate — среди открытых внегрупповых
// кандидатов проекта есть узел, чей down-корень — ДРУГОЙ (несвязанный) хост:
// ретро-присоединение обязано пропустить его (continue), присоединив только
// настоящего потомка искомого корня.
func TestOnRootOpenedSkipsNonMatchingCandidate(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)

	var matchInc int64
	mustScan(t, pool, &matchInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'disk','open',0,0,'',true) RETURNING id`, projectID, childHost)

	// Несвязанный хост без рёбер вообще — его down-корень отсутствует, он
	// НЕ должен зацепиться за rootHost при переборе.
	unrelatedHost := seedHost(t, pool, projectID, "unrelated-"+randSlug(t))
	var unrelatedInc int64
	mustScan(t, pool, &unrelatedInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'memory','open',0,0,'',true) RETURNING id`, projectID, unrelatedHost)

	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	g, _ := newGrouper(pool)
	if err := g.OnRootOpened(ctx, "host", rootInc, "host", rootHost, projectID); err != nil {
		t.Fatalf("OnRootOpened: %v", err)
	}
	var matchGroup *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, matchInc).Scan(&matchGroup); err != nil {
		t.Fatalf("read match group_id: %v", err)
	}
	if matchGroup == nil {
		t.Fatal("real descendant of the root must be retro-attached")
	}
	var unrelatedGroup *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, unrelatedInc).Scan(&unrelatedGroup); err != nil {
		t.Fatalf("read unrelated group_id: %v", err)
	}
	if unrelatedGroup != nil {
		t.Fatal("unrelated candidate (no path to the root) must be skipped, not attached")
	}
}

// TestOnRootOpenedSkipsAlreadyResolvedGroup — к моменту переборa кандидатов
// группа корня уже успела закрыться (sweep обогнал ретро-присоединение):
// OnRootOpened обязан выйти без ошибки, не присоединив кандидата.
func TestOnRootOpenedSkipsAlreadyResolvedGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)

	var candidateInc int64
	mustScan(t, pool, &candidateInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'disk','open',0,0,'',true) RETURNING id`, projectID, childHost)

	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	// Группа корня уже существует и уже закрыта (гонка со sweep).
	store := incidentgroup.NewStore(pool)
	if _, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost); err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}

	g, _ := newGrouper(pool)
	if err := g.OnRootOpened(ctx, "host", rootInc, "host", rootHost, projectID); err != nil {
		t.Fatalf("OnRootOpened: %v", err)
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, candidateInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatal("candidate must not be attached to an already-resolved group")
	}
}

// TestOnRootClosedResolvesGroup — при закрытии корня группа тоже закрывается.
func TestOnRootClosedResolvesGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)

	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	g, _ := newGrouper(pool)
	if err := g.OnRootClosed(ctx, "host", rootInc); err != nil {
		t.Fatalf("OnRootClosed: %v", err)
	}
	var resolved bool
	mustScanBool(t, pool, &resolved, `SELECT resolved_at IS NOT NULL FROM incident_groups WHERE id = $1`, grp.ID)
	if !resolved {
		t.Fatal("OnRootClosed must resolve the root's group")
	}
}

// TestOnRootClosedNoGroupIsNoop — если группа так и не была создана (членов
// не было), закрытие корня не должно быть ошибкой (см. комментарий в коде).
func TestOnRootClosedNoGroupIsNoop(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	g, _ := newGrouper(pool)
	if err := g.OnRootClosed(ctx, "host", 987654321); err != nil {
		t.Fatalf("OnRootClosed without an existing group must be a no-op, got %v", err)
	}
}

// mustScanBool — как mustScan, но для bool (несколько тестов файла читают
// булев результат напрямую).
func mustScanBool(t *testing.T, pool *pgxpool.Pool, dst *bool, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}

// TestAttachAlreadyGroupedSkipsEmptyGroup — W4: инцидент уже член ОТКРЫТОЙ
// группы g1 (root1); Attach резолвит down-корень в ДРУГОЙ root2 (fixedRoot-
// Resolver имитирует смену родителя между тиками) — SetGroup будет no-op
// (член уже в открытой группе), и группа root2 не должна создаваться вовсе:
// MemberEligible проверяется ДО EnsureGroup. Мутация (вернуть EnsureGroup
// перед проверкой) даёт лишнюю пустую группу — ловится сравнением
// len(before)/len(after) по OpenGroups, не только по attached.
func TestAttachAlreadyGroupedSkipsEmptyGroup(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost1 := seedHost(t, pool, projectID, "root1-"+randSlug(t))
	rootInc1 := seedSilent(t, pool, projectID, rootHost1, true)
	g1, err := store.EnsureGroup(ctx, projectID, "host", rootInc1, "host", rootHost1)
	if err != nil {
		t.Fatalf("EnsureGroup g1: %v", err)
	}

	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)
	if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g1.ID); err != nil || !ok {
		t.Fatalf("SetGroup g1: ok=%v err=%v", ok, err)
	}

	rootHost2 := seedHost(t, pool, projectID, "root2-"+randSlug(t))
	rootInc2 := seedSilent(t, pool, projectID, rootHost2, true)

	before, err := store.OpenGroups(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenGroups before: %v", err)
	}

	g := &incidentgroup.Grouper{
		Pool:  pool,
		Store: store,
		Roots: &fixedRootResolver{rootKind: "host", rootID: rootHost2, found: true},
	}
	attached, _, err := g.Attach(ctx, "host", memberInc, "host", memberHost)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if attached {
		t.Fatal("Attach must not report attached: member already in an open group")
	}

	after, err := store.OpenGroups(ctx, projectID)
	if err != nil {
		t.Fatalf("OpenGroups after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("Attach on an already-grouped member must not create an empty group for root2 (rootInc2=%d): before=%d after=%d",
			rootInc2, len(before), len(after))
	}

	var got int64
	mustScan(t, pool, &got, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc)
	if got != g1.ID {
		t.Fatalf("member must stay in g1 (%d), got %d", g1.ID, got)
	}
}

// TestOnRootOpenedReattachesFormerMember — W2: флапающий корень. Первое
// открытие присоединяет члена к группе g1; корень закрывается (g1
// резолвится, group_id члена НЕ сбрасывается — единственная запись о том,
// что упало вместе); корень открывается заново НОВЫМ инцидентом — ретро-
// перебор обязан подхватить того же члена (его group_id указывает на уже
// резолвнутую g1, значит он снова «внегрупповой» кандидат) и присоединить
// его к НОВОЙ группе g2, а не пропустить навсегда.
func TestOnRootOpenedReattachesFormerMember(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	childHost := seedHost(t, pool, projectID, "child-"+randSlug(t))
	seedEdgeHH(t, pool, projectID, rootHost, childHost)

	store := incidentgroup.NewStore(pool)
	g, _ := newGrouper(pool)

	rootInc1 := seedSilent(t, pool, projectID, rootHost, true)
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'disk','open',0,0,'',true) RETURNING id`, projectID, childHost)

	if err := g.OnRootOpened(ctx, "host", rootInc1, "host", rootHost, projectID); err != nil {
		t.Fatalf("OnRootOpened 1st: %v", err)
	}
	var group1 *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc).Scan(&group1); err != nil {
		t.Fatalf("read group_id 1: %v", err)
	}
	if group1 == nil {
		t.Fatalf("member must be attached after 1st OnRootOpened")
	}

	if _, err := pool.Exec(ctx, `UPDATE host_incidents SET status='resolved', resolved_at=now() WHERE id=$1`, rootInc1); err != nil {
		t.Fatalf("resolve root1: %v", err)
	}
	if ok, err := store.Resolve(ctx, "host", rootInc1); err != nil || !ok {
		t.Fatalf("Resolve group1: ok=%v err=%v", ok, err)
	}

	rootInc2 := seedSilent(t, pool, projectID, rootHost, true)
	if err := g.OnRootOpened(ctx, "host", rootInc2, "host", rootHost, projectID); err != nil {
		t.Fatalf("OnRootOpened 2nd: %v", err)
	}
	var group2 *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc).Scan(&group2); err != nil {
		t.Fatalf("read group_id 2: %v", err)
	}
	if group2 == nil {
		t.Fatal("member must be re-attached to the new group after the root re-opened")
	}
	if *group2 == *group1 {
		t.Fatalf("member must move to the NEW group, not stay pinned to the resolved one: got %d want != %d", *group2, *group1)
	}
}
