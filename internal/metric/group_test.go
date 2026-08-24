package metric_test

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/depsuppress"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/incidentgroup"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// newMetricGrouper — РЕАЛЬНЫЙ incidentgroup.Grouper поверх реального
// depsuppress.Suppressor (образец host/group_test.go, T4): интеграция
// metric↔группы тестируется без фейков резолвера корней. Присваивается в
// поле Evaluator.IncidentGroups структурно (duck-typing metricGroupHook).
func newMetricGrouper(pool *pgxpool.Pool) *incidentgroup.Grouper {
	return &incidentgroup.Grouper{
		Pool:  pool,
		Store: incidentgroup.NewStore(pool),
		Roots: depsuppress.NewSuppressor(pool),
	}
}

// seedGroupHost — хост проекта напрямую (пакету metric host.Store не нужен).
func seedGroupHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (project_id, name) VALUES ($1,$2) RETURNING id`,
		projectID, name).Scan(&id); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

// seedGroupDepEdge — явное ребро зависимости host(parent) -> host(child) (B5).
func seedGroupDepEdge(t *testing.T, pool *pgxpool.Pool, projectID, parentHostID, childHostID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO alert_dependencies (project_id, parent_host_id, child_host_id)
		VALUES ($1,$2,$3)`, projectID, parentHostID, childHostID); err != nil {
		t.Fatalf("seed dep edge: %v", err)
	}
}

// seedGroupSilentIncident — уже открытый silent-инцидент хоста, минуя
// host-оценщик; notified управляет гейтом «информирующего корня» (Р4).
func seedGroupSilentIncident(t *testing.T, pool *pgxpool.Pool, projectID, hostID int64, notified bool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, notified_open)
		VALUES ($1,$2,'silent','open',0,0,'',$3) RETURNING id`,
		projectID, hostID, notified).Scan(&id); err != nil {
		t.Fatalf("seed silent incident: %v", err)
	}
	return id
}

// seedHostLabeledGauge — gauge-точка с лейблом host в attributes: её ловит
// правило с label_key='host' (матчер идёт по attributes, не по колонке host).
func seedHostLabeledGauge(t *testing.T, ch driver.Conn, projectID int64, name, hostName string, val float64, ago time.Duration) {
	t.Helper()
	if err := ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', '', ?, ?, ?, 0, [], [], 0, '')`,
		projectID, name, map[string]string{"host": hostName},
		time.Now().UTC().Add(-ago), val); err != nil {
		t.Fatalf("seed labeled gauge: %v", err)
	}
}

// readMetricGroupID — group_id metric-инцидента (nil — вне групп).
func readMetricGroupID(t *testing.T, pool *pgxpool.Pool, incidentID int64) *int64 {
	t.Helper()
	var gid *int64
	if err := pool.QueryRow(context.Background(),
		`SELECT group_id FROM metric_incidents WHERE id = $1`, incidentID).Scan(&gid); err != nil {
		t.Fatalf("read group_id: %v", err)
	}
	return gid
}

// newGroupEvaluator — оценщик с реальным нотифаером поверх outbox (образец
// TestEvaluatorOpenCloseAlertOnce): наблюдаемое «уведомление ушло/не ушло» —
// задачи в outbox, как во всех тестах этого пакета.
func newGroupEvaluator(pool *pgxpool.Pool, ch driver.Conn, rules *metric.RuleService, incidents *metric.IncidentService, ob *notify.Outbox) *metric.Evaluator {
	return &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: &metric.MetricNotifier{Alerts: alert.NewService(pool), Outbox: ob, BaseURL: "https://gotcha.example", Incidents: incidents, Rules: rules, Pool: pool},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour, // тикер не используем — дёргаем Tick вручную
	}
}

// TestMetricMemberSilencedUnderInformingRoot — «metric-алерт хоста-ребёнка
// молчит и в составе» (сценарий 1 брифа): правило label_key='host' по хосту
// под упавшим ИНФОРМИРУЮЩИМ silent-корнем (notified_open=true) → инцидент
// открыт, присоединён к группе корня (group_id), step0 (задача в outbox) НЕ
// отправлен — информирует корень.
func TestMetricMemberSilencedUnderInformingRoot(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}

	rootID := seedGroupHost(t, pool, pid, "gw-01")
	childID := seedGroupHost(t, pool, pid, "web-01")
	seedGroupDepEdge(t, pool, pid, rootID, childID)
	rootInc := seedGroupSilentIncident(t, pool, pid, rootID, true)

	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: pid, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, LabelKey: "host", LabelValue: "web-01", Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	seedHostLabeledGauge(t, ch, pid, "cpu", "web-01", 150, time.Minute)

	ob := notify.NewOutbox(pool)
	eval := newGroupEvaluator(pool, ch, rules, incidents, ob)
	eval.IncidentGroups = newMetricGrouper(pool)

	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("metric incident должен открыться — группа глушит уведомление, не сам инцидент")
	}
	gid := readMetricGroupID(t, pool, in.ID)
	if gid == nil {
		t.Fatal("group_id IS NULL — член не присоединён к группе корня")
	}
	var gotRootInc int64
	if err := pool.QueryRow(ctx,
		`SELECT root_incident_id FROM incident_groups WHERE id = $1`, *gid).Scan(&gotRootInc); err != nil {
		t.Fatalf("read group root: %v", err)
	}
	if gotRootInc != rootInc {
		t.Errorf("группа якорится на инцидент %d, want silent-корень %d", gotRootInc, rootInc)
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 0 {
		t.Errorf("open jobs = %d, want 0 (информирует корень, step0 члена подавлен)", len(jobs))
	}
	if in.NotifiedOpen {
		t.Error("notified_open = true у члена информирующей группы, want false")
	}
}

// TestMetricUnknownHostNameStaysNoisy — fail-noisy по метке: правило
// label_key='host' на имя, которого нет в hosts проекта → AttachMetric не
// находит узел, attach не происходит, уведомление уходит штатно.
func TestMetricUnknownHostNameStaysNoisy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}

	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: pid, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, LabelKey: "host", LabelValue: "ghost-99", Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	seedHostLabeledGauge(t, ch, pid, "cpu", "ghost-99", 150, time.Minute)

	ob := notify.NewOutbox(pool)
	eval := newGroupEvaluator(pool, ch, rules, incidents, ob)
	eval.IncidentGroups = newMetricGrouper(pool)

	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil || !open {
		t.Fatalf("OpenFor: open=%v err=%v", open, err)
	}
	if readMetricGroupID(t, pool, in.ID) != nil {
		t.Error("group_id проставлен по неизвестному имени хоста, want вне групп")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Errorf("open jobs = %d, want 1 (метка не резолвится в узел — шумим как без D3)", len(jobs))
	}
}

// TestMetricOpenUnackedGroupGating — анти-залповый OpenUnacked (зеркало
// host T4/3-4 на metric_incidents): член ОТКРЫТОЙ группы исключён из выборки
// планировщика (Р5); после Resolve группы — вернулся, и его StartedAt =
// GREATEST(started_at, resolved_at) — лесенка бывшего члена стартует от
// момента освобождения, а не от started_at трёхчасовой давности (BLOCKER-1).
func TestMetricOpenUnackedGroupGating(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedProject(t, pool)
	rootID := seedGroupHost(t, pool, pid, "gw-01")

	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: pid, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, LabelKey: "host", LabelValue: "web-01", Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}
	member, created, err := incidents.Open(ctx, rule.ID, pid, 150, false, "")
	if err != nil || !created {
		t.Fatalf("Open member: created=%v err=%v", created, err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE metric_incidents SET started_at = now() - interval '3 hours' WHERE id = $1`, member.ID); err != nil {
		t.Fatalf("age member: %v", err)
	}

	rootInc := seedGroupSilentIncident(t, pool, pid, rootID, true)
	store := incidentgroup.NewStore(pool)
	grp, err := store.EnsureGroup(ctx, pid, "host", rootInc, "host", rootID)
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if err := store.SetGroup(ctx, "metric", member.ID, grp.ID); err != nil {
		t.Fatalf("SetGroup: %v", err)
	}

	contains := func(list []escalation.PendingIncident, id int64) *escalation.PendingIncident {
		for i := range list {
			if list[i].ID == id {
				return &list[i]
			}
		}
		return nil
	}

	list, err := incidents.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked (группа открыта): %v", err)
	}
	if contains(list, member.ID) != nil {
		t.Error("член ОТКРЫТОЙ группы попал в OpenUnacked — планировщик эскалировал бы его в обход корня")
	}

	if ok, err := store.Resolve(ctx, "host", rootInc); err != nil || !ok {
		t.Fatalf("Resolve group: ok=%v err=%v", ok, err)
	}
	list, err = incidents.OpenUnacked(ctx)
	if err != nil {
		t.Fatalf("OpenUnacked (группа закрыта): %v", err)
	}
	got := contains(list, member.ID)
	if got == nil {
		t.Fatalf("член закрытой группы не вернулся в OpenUnacked — досылка step0 после распада группы потеряна: %+v", list)
	}
	if age := time.Since(got.StartedAt); age >= time.Minute {
		t.Errorf("StartedAt отстаёт на %v — база отсчёта лесенки не GREATEST(started_at, resolved_at): step1 с delay>0 на следующем тике стал бы дью и дал залп", age)
	}
}
