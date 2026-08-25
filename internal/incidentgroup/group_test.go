package incidentgroup_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func randSlug(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

func mustScan(t *testing.T, pool *pgxpool.Pool, dst *int64, sql string, args ...any) {
	t.Helper()
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("scan %q: %v", sql, err)
	}
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// seedProject — организация + проект.
func seedProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var orgID, projectID int64
	slug := "ig-" + randSlug(t)
	mustScan(t, pool, &orgID,
		`INSERT INTO organizations (slug,name,event_quota) VALUES ($1,$1,0) RETURNING id`, slug)
	mustScan(t, pool, &projectID,
		`INSERT INTO projects (org_id,slug,name) VALUES ($1,$2,$2) RETURNING id`, orgID, slug)
	return projectID
}

func seedHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	var hostID int64
	mustScan(t, pool, &hostID,
		`INSERT INTO hosts (project_id, name, environment, role) VALUES ($1,$2,'','') RETURNING id`,
		projectID, name)
	return hostID
}

// seedSilent — открытый silent-инцидент хоста; notified управляет гейтом
// «информирующего корня».
func seedSilent(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, notified bool) int64 {
	t.Helper()
	var id int64
	mustScan(t, pool, &id, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',$3) RETURNING id`, projectID, hostID, notified)
	return id
}

func TestEnsureGroupIdempotentAndResolve(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "root-"+randSlug(t))
	incID := seedSilent(t, pool, projectID, hostID, true)

	store := incidentgroup.NewStore(pool)
	g1, err := store.EnsureGroup(ctx, projectID, "host", incID, "host", hostID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	g2, err := store.EnsureGroup(ctx, projectID, "host", incID, "host", hostID)
	if err != nil {
		t.Fatalf("EnsureGroup 2nd: %v", err)
	}
	if g1.ID != g2.ID {
		t.Fatalf("EnsureGroup not idempotent: %d != %d", g1.ID, g2.ID)
	}
	ok, err := store.Resolve(ctx, "host", incID)
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	ok, err = store.Resolve(ctx, "host", incID)
	if err != nil || ok {
		t.Fatalf("Resolve second call must be no-op: ok=%v err=%v", ok, err)
	}
}

func TestSetGroupFirstWriteWins(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	hostID := seedHost(t, pool, projectID, "h-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, hostID, true)
	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	store := incidentgroup.NewStore(pool)
	g1, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", hostID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.SetGroup(ctx, projectID, "host", memberInc, g1.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}
	// Повторный attach к другой группе — no-op (первый выигрывает).
	if _, err := store.SetGroup(ctx, projectID, "host", memberInc, g1.ID+1000); err != nil {
		t.Fatalf("SetGroup 2nd: %v", err)
	}
	var got int64
	mustScan(t, pool, &got, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc)
	if got != g1.ID {
		t.Fatalf("first attach must win: got group_id=%d want %d", got, g1.ID)
	}
	if _, err := store.SetGroup(ctx, projectID, "trace", memberInc, g1.ID); err == nil {
		t.Fatalf("unknown source must error")
	}
}

// TestSetGroupDoesNotOverwriteOpenMembership — W2: инцидент уже член
// ОТКРЫТОЙ группы g1; SetGroup к РЕАЛЬНОЙ (не рандомной) открытой группе g2
// другого корня — no-op (ok=false), членство в g1 не меняется. Отличие от
// TestSetGroupFirstWriteWins: там g1.ID+1000 — заведомо несуществующая
// группа; здесь g2 реальна и открыта, проверяет именно ветку «старая группа
// ещё открыта — не перезаписывать».
func TestSetGroupDoesNotOverwriteOpenMembership(t *testing.T) {
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
	rootHost2 := seedHost(t, pool, projectID, "root2-"+randSlug(t))
	rootInc2 := seedSilent(t, pool, projectID, rootHost2, true)
	g2, err := store.EnsureGroup(ctx, projectID, "host", rootInc2, "host", rootHost2)
	if err != nil {
		t.Fatalf("EnsureGroup g2: %v", err)
	}

	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g1.ID); err != nil || !ok {
		t.Fatalf("SetGroup g1: ok=%v err=%v", ok, err)
	}
	ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g2.ID)
	if err != nil {
		t.Fatalf("SetGroup g2: %v", err)
	}
	if ok {
		t.Fatalf("SetGroup must not overwrite membership in an OPEN group (g2=%d)", g2.ID)
	}
	var got int64
	mustScan(t, pool, &got, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc)
	if got != g1.ID {
		t.Fatalf("member must stay in g1 (%d), got %d", g1.ID, got)
	}
}

// TestSetGroupCompositionCrossProjectNoop — W6: SetGroup с project_id
// чужого проекта не присоединяет чужой инцидент (WHERE не матчит строку);
// Composition с project_id чужого проекта отдаёт пустой список, хотя группа
// реальна и с членом — обе функции проверяют tenant-изоляцию прямо в
// запросе, не полагаясь только на инварианты вызывающих. Группа собрана из
// членов ВСЕХ ЧЕТЫРЁХ источников (фикс-раунд R1b, MAJOR-2): ревьюер снял
// фильтр project_id разом в ветках uptime/metric/slo у feedMemberSelect, а
// прежняя версия теста держала в группе только host-члена — мутация осталась
// незамеченной. Одного host-члена мало: он единственный источник, чья
// project_id-ветка и так была покрыта.
func TestSetGroupCompositionCrossProjectNoop(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	otherProjectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	memberHost := seedHost(t, pool, projectID, "m-"+randSlug(t))
	var memberInc int64
	mustScan(t, pool, &memberInc, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail)
		VALUES ($1,$2,'disk','open',0,0,'') RETURNING id`, projectID, memberHost)

	ok, err := store.SetGroup(ctx, otherProjectID, "host", memberInc, g.ID)
	if err != nil {
		t.Fatalf("SetGroup cross-project: %v", err)
	}
	if ok {
		t.Fatal("SetGroup with a foreign project_id must be a no-op")
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM host_incidents WHERE id = $1`, memberInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatalf("cross-project SetGroup must not attach, got group_id=%d", *groupID)
	}

	if ok, err := store.SetGroup(ctx, projectID, "host", memberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup own project: ok=%v err=%v", ok, err)
	}

	// uptime-член.
	var monitorID, uptimeMemberInc int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &uptimeMemberInc, `
		INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)
	if ok, err := store.SetGroup(ctx, projectID, "uptime", uptimeMemberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup uptime own project: ok=%v err=%v", ok, err)
	}

	// metric-член.
	var ruleID, metricMemberInc int64
	mustScan(t, pool, &ruleID, `
		INSERT INTO metric_alert_rules (project_id, metric_name, aggregation, comparator, threshold)
		VALUES ($1,'cpu.load','avg','gt',0.9) RETURNING id`, projectID)
	mustScan(t, pool, &metricMemberInc, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1,$2,1,1) RETURNING id`, ruleID, projectID)
	if ok, err := store.SetGroup(ctx, projectID, "metric", metricMemberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup metric own project: ok=%v err=%v", ok, err)
	}

	// slo-член.
	var sloID, sloMemberInc int64
	mustScan(t, pool, &sloID, `
		INSERT INTO slos (project_id, name, sli_kind, target, window_days)
		VALUES ($1,'slo-`+randSlug(t)+`','availability',0.99,30) RETURNING id`, projectID)
	mustScan(t, pool, &sloMemberInc, `
		INSERT INTO slo_incidents (slo_id, project_id, burn_rate)
		VALUES ($1,$2,20) RETURNING id`, sloID, projectID)
	if ok, err := store.SetGroup(ctx, projectID, "slo", sloMemberInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup slo own project: ok=%v err=%v", ok, err)
	}

	members, err := store.Composition(ctx, otherProjectID, g.ID)
	if err != nil {
		t.Fatalf("Composition cross-project: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("Composition with a foreign project_id must return nothing, got %d members", len(members))
	}

	members, err = store.Composition(ctx, projectID, g.ID)
	if err != nil {
		t.Fatalf("Composition own project: %v", err)
	}
	if len(members) != 4 {
		t.Fatalf("Composition own project must return all 4 members, got %d", len(members))
	}
}

// TestSetGroupCrossProjectUptime — W6: `incidents` (uptime) не имеет своей
// колонки project_id — sourceMeta.projectCond идёт через monitors (EXISTS); та же
// проверка, что TestSetGroupCompositionCrossProjectNoop, но для ветки,
// которую легко сломать по-другому (забыть JOIN на monitors).
func TestSetGroupCrossProjectUptime(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	projectID := seedProject(t, pool)
	otherProjectID := seedProject(t, pool)
	store := incidentgroup.NewStore(pool)

	rootHost := seedHost(t, pool, projectID, "root-"+randSlug(t))
	rootInc := seedSilent(t, pool, projectID, rootHost, true)
	g, err := store.EnsureGroup(ctx, projectID, "host", rootInc, "host", rootHost)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	var monitorID, uptimeInc int64
	mustScan(t, pool, &monitorID, `
		INSERT INTO monitors (project_id, name, kind, interval_seconds)
		VALUES ($1,'mon-`+randSlug(t)+`','http',60) RETURNING id`, projectID)
	mustScan(t, pool, &uptimeInc, `
		INSERT INTO incidents (monitor_id, notified_open) VALUES ($1,true) RETURNING id`, monitorID)

	ok, err := store.SetGroup(ctx, otherProjectID, "uptime", uptimeInc, g.ID)
	if err != nil {
		t.Fatalf("SetGroup cross-project uptime: %v", err)
	}
	if ok {
		t.Fatal("SetGroup(uptime) with a foreign project_id must be a no-op")
	}
	var groupID *int64
	if err := pool.QueryRow(ctx, `SELECT group_id FROM incidents WHERE id = $1`, uptimeInc).Scan(&groupID); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	if groupID != nil {
		t.Fatalf("cross-project SetGroup(uptime) must not attach, got group_id=%d", *groupID)
	}

	if ok, err := store.SetGroup(ctx, projectID, "uptime", uptimeInc, g.ID); err != nil || !ok {
		t.Fatalf("SetGroup(uptime) own project: ok=%v err=%v", ok, err)
	}
}
