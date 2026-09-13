package host_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/host"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

var evalSeq atomic.Int64

type fakeNotifier struct {
	mu       sync.Mutex
	opened   []host.Incident
	resolved []host.Incident

	err error
}

func (f *fakeNotifier) HostIncidentOpened(_ context.Context, in host.Incident, _ host.Host, _ host.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, in)
	return f.err
}

func (f *fakeNotifier) HostIncidentResolved(_ context.Context, in host.Incident, _ host.Host) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, in)
	return f.err
}

func (f *fakeNotifier) NotifyStep(_ context.Context, incidentID int64, channelIDs []int64, _ int) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, host.Incident{ID: incidentID})
	if f.err != nil {
		return nil, f.err
	}
	return channelIDs, nil
}

func (f *fakeNotifier) NotifyRecovery(_ context.Context, incidentID int64, _ []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, host.Incident{ID: incidentID})
	return f.err
}

func (f *fakeNotifier) openedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opened)
}

func (f *fakeNotifier) resolvedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.resolved)
}

func seedEvalProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	n := evalSeq.Add(1)
	var orgID, projectID int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO organizations (slug, name, event_quota) VALUES ($1,'Eval',1000000) RETURNING id",
		fmt.Sprintf("host-eval-%d", n)).Scan(&orgID); err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name) VALUES ($1,$2,'API') RETURNING id",
		orgID, fmt.Sprintf("api-%d", n)).Scan(&projectID); err != nil {
		t.Fatalf("project: %v", err)
	}
	return projectID
}

func seedAlertChannel(t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	asvc := alert.NewService(pool)
	if _, err := asvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("seed alert channel: %v", err)
	}
}

func seedEvalHost(t *testing.T, pool *pgxpool.Pool, projectID int64, name string) host.Host {
	t.Helper()
	ctx := context.Background()
	store := host.NewStore(pool)
	if _, err := store.Upsert(ctx, projectID, entries(name)); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	h, ok, err := store.Get(ctx, projectID, name)
	if err != nil || !ok {
		t.Fatalf("get host: ok=%v err=%v", ok, err)
	}
	return h
}

func setHostLastSeen(t *testing.T, pool *pgxpool.Pool, hostID int64, lastSeen time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE hosts SET last_seen = $1, first_seen = $2 WHERE id = $3",
		lastSeen, lastSeen.Add(-24*time.Hour), hostID); err != nil {
		t.Fatalf("set last_seen: %v", err)
	}
}

func setHostFirstSeen(t *testing.T, pool *pgxpool.Pool, hostID int64, firstSeen time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE hosts SET first_seen = $1 WHERE id = $2", firstSeen, hostID); err != nil {
		t.Fatalf("set first_seen: %v", err)
	}
}

func seedHostMetricPoint(t *testing.T, ch driver.Conn, projectID int64, name, hostName string, attrs map[string]string, val float64, ago time.Duration) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if err := ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, host, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', '', ?, ?, ?, ?, 0, [], [], 0, '')`,
		projectID, name, hostName, attrs, time.Now().UTC().Add(-ago), val); err != nil {
		t.Fatalf("seed host metric %s: %v", name, err)
	}
}

func newEvaluator(pool *pgxpool.Pool, ch driver.Conn, notifier host.Notifier) *host.Evaluator {
	return &host.Evaluator{
		Store:     host.NewStore(pool),
		Settings:  host.NewSettingsService(pool),
		Incidents: host.NewIncidentService(pool),
		Metrics:   metric.NewQuery(ch),
		Overrides: host.NewHostOverrideService(pool),
		Groups:    host.NewGroupThresholdService(pool),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
		Pool:      pool,
		Interval:  time.Hour,
		StartedAt: time.Now().UTC().Add(-24 * time.Hour),
	}
}

func TestEvaluatorDiskOpensWithWorstMountpointDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/var"}, 0.60, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open after 0.95 > 0.90")
	}
	if in.Detail != "/" {
		t.Errorf("Detail = %q, want худший mountpoint %q", in.Detail, "/")
	}
	if notifier.openedCount() != 1 {
		t.Errorf("opened notifications = %d, want 1", notifier.openedCount())
	}
}

func TestEvaluatorDiskRetickBumpsSilently(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}
	if notifier.openedCount() != 1 {
		t.Fatalf("opened after first tick = %d, want 1", notifier.openedCount())
	}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if notifier.openedCount() != 1 {
		t.Errorf("opened after second tick = %d, want 1 (bump, не повторное открытие)", notifier.openedCount())
	}
	if notifier.resolvedCount() != 0 {
		t.Errorf("resolved after second tick = %d, want 0", notifier.resolvedCount())
	}
}

func TestEvaluatorDiskRecoveryResolves(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.50, time.Minute)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Error("disk incident must be resolved after recovery to 0.50")
	}
	if notifier.resolvedCount() != 1 {
		t.Errorf("resolved notifications = %d, want 1", notifier.resolvedCount())
	}
}

func TestEvaluatorNoMetricDataNoIncident(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "silent-metric-host")

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Error("disk incident open despite no metric data at all")
	}
}

func TestEvaluatorMemoryRequiresUsedStateMatcher(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.memory.utilization", h.Name, map[string]string{"state": "free"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "memory")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Fatal("memory incident open on state=free point — матчер state=used не применён")
	}

	seedHostMetricPoint(t, ch, pid, "system.memory.utilization", h.Name, map[string]string{"state": "used"}, 0.95, time.Minute)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	_, open, err = incidents.OpenFor(ctx, h.ID, "memory")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Error("memory incident must open once state=used point breaches threshold")
	}
}

func TestEvaluatorLoadDividesByCoresCoresMissingSkips(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.cpu.load_average.5m", h.Name, nil, 8, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "load")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Fatal("load incident open despite missing cores metric — cores отсутствует, оценивать нечем")
	}

	seedHostMetricPoint(t, ch, pid, "system.cpu.logical.count", h.Name, nil, 2, time.Minute)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	_, open, err = incidents.OpenFor(ctx, h.ID, "load")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Error("load incident must open: 8/2=4 > порог 2.0")
	}
}

func TestEvaluatorSilentOpensAndResolvesOnUpsert(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "silent-01")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("silent incident must open: 10 минут тишины > дефолтного порога 5 минут")
	}

	if _, err := host.NewStore(pool).Upsert(ctx, pid, []host.TouchEntry{{Name: h.Name}}); err != nil {
		t.Fatalf("upsert (host came back): %v", err)
	}
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}
	_, open, err = incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Error("silent incident must resolve once host returns (last_seen обновлён)")
	}
	if notifier.resolvedCount() != 1 {
		t.Errorf("resolved notifications = %d, want 1", notifier.resolvedCount())
	}
}

func TestEvaluatorSilentResolvesInsideHysteresisDeadZone(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "silent-deadzone")

	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-310*time.Second))
	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("silent incident must open at 310s тишины > порога 300с")
	}

	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-290*time.Second))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick dead zone: %v", err)
	}

	_, open, err = incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Error("silent incident остался открытым на 290с тишины (<= порога 300с) — похоже, применён гистерезис Decide вместо прямого сравнения (design.md §4.4)")
	}
	if notifier.resolvedCount() != 1 {
		t.Errorf("resolved notifications = %d, want 1", notifier.resolvedCount())
	}
}

func TestEvaluatorDiskDisabledSettingSkipsEvaluation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.99, time.Minute)

	settings := host.NewSettingsService(pool)
	s := host.DefaultSettings()
	s.DiskEnabled = false
	if err := settings.Save(ctx, pid, s); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Error("disk incident open despite disk_enabled=false")
	}
}

func TestEvaluatorOneHostFailureDoesNotBlockNeighbor(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	empty := seedEvalHost(t, pool, pid, "no-metrics-host")
	full := seedEvalHost(t, pool, pid, "with-metrics-host")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", full.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, openEmpty, err := incidents.OpenFor(ctx, empty.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor empty: %v", err)
	}
	if openEmpty {
		t.Error("хост без метрик неожиданно открыл disk-инцидент")
	}
	_, openFull, err := incidents.OpenFor(ctx, full.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor full: %v", err)
	}
	if !openFull {
		t.Error("сосед с реальным нарушением не открыл disk-инцидент — обработка одного хоста заблокировала другой")
	}
}

func TestEvaluatorSilentGraceAfterStart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "silent-after-restart")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-30*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.StartedAt = time.Now().UTC().Add(-time.Minute)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick сразу после старта: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Fatal("silent открыт на первом тике после старта — тишина за наш простой засчитана хосту")
	}
	if notifier.openedCount() != 0 {
		t.Errorf("уведомлений об открытии = %d, want 0", notifier.openedCount())
	}

	eval.StartedAt = time.Now().UTC().Add(-30 * time.Minute)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick после грейса: %v", err)
	}
	_, open, err = incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor после грейса: %v", err)
	}
	if !open {
		t.Error("silent не открыт после того, как оценщик отработал дольше порога")
	}
}

func TestEvaluatorSilentSkipsEphemeralHost(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "pod-ephemeral")
	lastSeen := time.Now().UTC().Add(-30 * time.Minute)
	setHostLastSeen(t, pool, h.ID, lastSeen)
	setHostFirstSeen(t, pool, h.ID, lastSeen.Add(-time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if open {
		t.Fatal("silent открыт по хосту, наблюдавшемуся меньше порога тишины (эфемерный под)")
	}

	setHostFirstSeen(t, pool, h.ID, lastSeen.Add(-24*time.Hour))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	_, open, err = incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor 2: %v", err)
	}
	if !open {
		t.Error("silent не открыт по давно наблюдаемому хосту — защита от эфемерных задела обычный сервер")
	}
}

func TestEvaluatorSilentDoesNotBumpEveryTick(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "silent-no-bump")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	eval := newEvaluator(pool, ch, &fakeNotifier{})
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}
	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil || !open {
		t.Fatalf("OpenFor: open=%v err=%v", open, err)
	}

	xmin := func() string {
		var v string
		if err := pool.QueryRow(ctx,
			"SELECT xmin::text FROM host_incidents WHERE id = $1", in.ID).Scan(&v); err != nil {
			t.Fatalf("xmin: %v", err)
		}
		return v
	}
	before := xmin()
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}
	if after := xmin(); after != before {
		t.Errorf("строка инцидента переписана на повторном тике (xmin %s → %s)", before, after)
	}

	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-60*time.Minute))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 3: %v", err)
	}
	if after := xmin(); after == before {
		t.Error("шестикратный рост тишины не обновил инцидент — peak перестал отражать максимум")
	}
}

type stuckCH struct {
	driver.Conn
	calls atomic.Int64
}

func (c *stuckCH) QueryRow(ctx context.Context, _ string, _ ...any) driver.Row {
	c.calls.Add(1)
	<-ctx.Done()
	return stuckRow{err: ctx.Err()}
}

func (c *stuckCH) Query(ctx context.Context, _ string, _ ...any) (driver.Rows, error) {
	c.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

type stuckRow struct{ err error }

func (r stuckRow) Err() error           { return r.err }
func (r stuckRow) Scan(...any) error    { return r.err }
func (r stuckRow) ScanStruct(any) error { return r.err }

func TestEvaluatorSilentEvaluatedWhileClickHouseHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "silent-while-ch-hangs")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-30*time.Minute))
	seedEvalHost(t, pool, pid, "noisy-neighbour")

	stuck := &stuckCH{}
	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, nil, notifier)
	eval.Metrics = metric.NewQuery(stuck)
	eval.Interval = time.Second

	done := make(chan error, 1)
	go func() { done <- eval.Tick(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tick: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Tick не завершился: повисший ClickHouse блокирует оценщик")
	}

	if stuck.calls.Load() == 0 {
		t.Error("оценщик не ходил в ClickHouse вовсе — тест не проверяет то, что должен")
	}
	if got := eval.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по дедлайну тика, want 0", got)
	}
	if got := eval.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Error("silent не открыт при недоступном ClickHouse — тишина заперта за CH-запросами")
	}
	if notifier.openedCount() != 1 {
		t.Errorf("уведомлений об открытии = %d, want 1", notifier.openedCount())
	}
}

// projectStuckCH как stuckCH, но пишет project_id (первый аргумент запроса) —
// так видно, какой хост (у каждого своя project_id) дошёл до ClickHouse.
type projectStuckCH struct {
	driver.Conn
	mu       sync.Mutex
	projects []int64
}

func (c *projectStuckCH) record(args []any) {
	if len(args) == 0 {
		return
	}
	pid, ok := args[0].(int64)
	if !ok {
		return
	}
	c.mu.Lock()
	c.projects = append(c.projects, pid)
	c.mu.Unlock()
}

func (c *projectStuckCH) QueryRow(ctx context.Context, _ string, args ...any) driver.Row {
	c.record(args)
	<-ctx.Done()
	return stuckRow{err: ctx.Err()}
}

func (c *projectStuckCH) Query(ctx context.Context, _ string, args ...any) (driver.Rows, error) {
	c.record(args)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *projectStuckCH) touchedProjects() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int64(nil), c.projects...)
}

// Без ротации бюджет тика (пол 10с) всегда обрывается на одном и том же первом хосте.
func TestEvaluatorRotatesThresholdPassAcrossTicks(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)

	var projectIDs []int64
	for _, name := range []string{"rot-a", "rot-b", "rot-c"} {
		pid := seedEvalProject(t, pool)
		projectIDs = append(projectIDs, pid)
		seedEvalHost(t, pool, pid, name)
	}

	stuck := &projectStuckCH{}
	eval := newEvaluator(pool, nil, &fakeNotifier{})
	eval.Metrics = metric.NewQuery(stuck)
	eval.Interval = time.Second // budget: пол minTickBudget = 10с

	firstTouched := func(tickNo int) int64 {
		before := len(stuck.touchedProjects())
		if err := eval.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", tickNo, err)
		}
		got := stuck.touchedProjects()[before:]
		if len(got) == 0 {
			t.Fatalf("tick %d: ClickHouse ни разу не запрошен — тест не проверяет то, что должен", tickNo)
		}
		first := got[0]
		for _, pid := range got {
			if pid != first {
				t.Fatalf("tick %d: за один тик порогового прохода задет не один хост: %v", tickNo, got)
			}
		}
		return first
	}

	// За 4 тика курсор обязан пройти все три хоста по кругу и вернуться к первому.
	want := []int64{projectIDs[0], projectIDs[1], projectIDs[2], projectIDs[0]}
	for i, w := range want {
		got := firstTouched(i + 1)
		if got != w {
			t.Fatalf("тик %d: обработан хост проекта %d, want %d (порядок %v)", i+1, got, w, projectIDs)
		}
		if skipped := eval.LastTickSkippedHosts(); skipped != 2 {
			t.Errorf("тик %d: LastTickSkippedHosts() = %d, want 2 (два хоста из трёх не влезли в бюджет)", i+1, skipped)
		}
	}
}

type countingCH struct {
	driver.Conn
	typeQueries atomic.Int64
}

func (c *countingCH) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	if strings.Contains(query, "any(type)") {
		c.typeQueries.Add(1)
	}
	return c.Conn.QueryRow(ctx, query, args...)
}

func TestEvaluatorCachesMetricTypePerTick(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	first := seedEvalHost(t, pool, pid, "cache-01")
	second := seedEvalHost(t, pool, pid, "cache-02")
	for _, h := range []host.Host{first, second} {
		seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.10, time.Minute)
		seedHostMetricPoint(t, ch, pid, "system.memory.utilization", h.Name, map[string]string{"state": "used"}, 0.10, time.Minute)
		seedHostMetricPoint(t, ch, pid, "system.cpu.load_average.5m", h.Name, nil, 0.5, time.Minute)
		seedHostMetricPoint(t, ch, pid, "system.cpu.logical.count", h.Name, nil, 4, time.Minute)
	}

	counting := &countingCH{Conn: ch}
	eval := newEvaluator(pool, nil, &fakeNotifier{})
	eval.Metrics = metric.NewQuery(counting)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := counting.typeQueries.Load(); got != 4 {
		t.Errorf("запросов типа метрики = %d, want 4 (по одному на метрику на весь тик, а не на каждый хост)", got)
	}
}

func TestEvaluatorPublishesTickLiveness(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedEvalHost(t, pool, pid, "liveness-01")

	eval := newEvaluator(pool, ch, &fakeNotifier{})
	if got := eval.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	before := time.Now().Unix()
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := eval.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := eval.LastTickSeconds(); got <= 0 || got > 60 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

type cancellingNotifier struct {
	fakeNotifier
	cancel   context.CancelFunc
	seenErrs []error
}

func (n *cancellingNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, step int) ([]int64, error) {
	enqueued, err := n.fakeNotifier.NotifyStep(ctx, incidentID, channelIDs, step)
	n.mu.Lock()
	n.seenErrs = append(n.seenErrs, ctx.Err())
	n.mu.Unlock()
	n.cancel()
	return enqueued, err
}

func (n *cancellingNotifier) notifierCtxErrs() []error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]error(nil), n.seenErrs...)
}

func TestEvaluatorNotifiesEvenWhenTickBudgetRunsOut(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "silent-budget-out")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-30*time.Minute))

	notifier := &cancellingNotifier{cancel: cancel}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if notifier.openedCount() != 1 {
		t.Fatalf("уведомлений об открытии = %d, want 1", notifier.openedCount())
	}
	for i, err := range notifier.notifierCtxErrs() {
		if err != nil {
			t.Errorf("нотифаер #%d получил уже отменённый контекст (%v) — задача в outbox не встанет", i, err)
		}
	}

	var notifiedOpen bool
	if err := pool.QueryRow(context.Background(),
		"SELECT notified_open FROM host_incidents WHERE host_id = $1 AND kind = 'silent'",
		h.ID).Scan(&notifiedOpen); err != nil {
		t.Fatalf("read notified_open: %v", err)
	}
	if !notifiedOpen {
		t.Error("notified_open=false после отмены контекста тика — уведомление потеряно навсегда (досылки по флагу в host нет)")
	}
}

func TestEvaluatorKeepsNotifiedFalseWhenNotifierFails(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "silent-notify-fails")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-30*time.Minute))

	notifier := &fakeNotifier{err: errors.New("enqueue failed")}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if notifier.openedCount() != 1 {
		t.Fatalf("вызовов нотификатора об открытии = %d, want 1", notifier.openedCount())
	}
	var notifiedOpen bool
	if err := pool.QueryRow(ctx,
		"SELECT notified_open FROM host_incidents WHERE host_id = $1 AND kind = 'silent'",
		h.ID).Scan(&notifiedOpen); err != nil {
		t.Fatalf("read notified_open: %v", err)
	}
	if notifiedOpen {
		t.Error("notified_open=true при провале постановки в очередь — флаг врёт оператору")
	}
}

func TestEvaluatorHostOverrideOpensBelowProjectThreshold(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.60, time.Minute)

	overrides := host.NewHostOverrideService(pool)
	on := true
	threshold := 0.50
	if err := overrides.Save(ctx, h.ID, host.ThresholdOverride{DiskEnabled: &on, DiskThreshold: &threshold}); err != nil {
		t.Fatalf("save override: %v", err)
	}

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Error("disk incident must be open at 60% против эффективного (host-override) порога 0.50 — проектный 0.90 при этой утилизации не открыл бы")
	}
}

func TestEvaluatorDisablingViaOverrideResolvesOpenIncident(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor after tick 1: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open after 0.95 > 0.90 (setup)")
	}

	overrides := host.NewHostOverrideService(pool)
	off := false
	if err := overrides.Save(ctx, h.ID, host.ThresholdOverride{DiskEnabled: &off}); err != nil {
		t.Fatalf("save override: %v", err)
	}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM host_incidents WHERE id = $1", in.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "resolved" {
		t.Errorf("status = %q, want resolved (M-A: выключенный override'ом вид должен закрыть открытый инцидент хоста)", status)
	}
}

func TestEvaluatorGroupThresholdOpensBelowProjectThreshold(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	store := host.NewStore(pool)
	if _, err := store.Upsert(ctx, pid, []host.TouchEntry{{Name: "web-01", Role: "web"}}); err != nil {
		t.Fatalf("upsert host with role: %v", err)
	}
	h, ok, err := store.Get(ctx, pid, "web-01")
	if err != nil || !ok {
		t.Fatalf("get host: ok=%v err=%v", ok, err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.60, time.Minute)

	groups := host.NewGroupThresholdService(pool)
	on := true
	threshold := 0.50
	if err := groups.Upsert(ctx, pid, "role", "web", host.ThresholdOverride{DiskEnabled: &on, DiskThreshold: &threshold}); err != nil {
		t.Fatalf("upsert group threshold: %v", err)
	}

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	_, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Error("disk incident must be open at 60% против эффективного (role-group) порога 0.50 — проектный дефолт 0.90 при этой утилизации не открыл бы")
	}
}

func TestEvaluatorDisablingViaOverrideResolvesOpenSilentIncident(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "silent-02")
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-10*time.Minute))

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 1: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "silent")
	if err != nil {
		t.Fatalf("OpenFor after tick 1: %v", err)
	}
	if !open {
		t.Fatal("silent incident must be open after 10 min silence (setup)")
	}

	overrides := host.NewHostOverrideService(pool)
	off := false
	if err := overrides.Save(ctx, h.ID, host.ThresholdOverride{SilentEnabled: &off}); err != nil {
		t.Fatalf("save override: %v", err)
	}

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 2: %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, "SELECT status FROM host_incidents WHERE id = $1", in.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "resolved" {
		t.Errorf("status = %q, want resolved (M-A silent-ветка: выключенный override'ом silent должен закрыть открытый инцидент хоста)", status)
	}
}

type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

func TestEvaluatorMaintenanceSuppressesThresholdNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Maint = mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil })

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open after 0.95 > 0.90 even in maintenance")
	}
	if !in.InMaintenance {
		t.Error("Incident.InMaintenance = false, want true")
	}
	if notifier.openedCount() != 0 {
		t.Errorf("opened notifications = %d, want 0 (suppressed by maintenance)", notifier.openedCount())
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.50, time.Minute)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}

	_, open, err = incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor after resolve: %v", err)
	}
	if open {
		t.Error("disk incident must be resolved after recovery to 0.50")
	}
	if notifier.resolvedCount() != 0 {
		t.Errorf("resolved notifications = %d, want 0 (suppressed by maintenance)", notifier.resolvedCount())
	}
}

func TestEvaluatorMaintenanceFalseStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	seedAlertChannel(t, pool, pid)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	eval.Maint = mockMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil })

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open after 0.95 > 0.90")
	}
	if in.InMaintenance {
		t.Error("Incident.InMaintenance = true, want false (outside window)")
	}
	if notifier.openedCount() != 1 {
		t.Errorf("opened notifications = %d, want 1 (not suppressed outside maintenance)", notifier.openedCount())
	}
}

func TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	inWindow := true
	eval.Maint = mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil })

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open after 0.95 > 0.90 even in maintenance")
	}
	if !in.InMaintenance {
		t.Fatal("Incident.InMaintenance = false, want true (open must persist the flag)")
	}
	if notifier.openedCount() != 0 {
		t.Fatalf("opened notifications = %d, want 0 (suppressed by maintenance)", notifier.openedCount())
	}

	inWindow = false

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.50, time.Minute)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}

	_, open, err = incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor after resolve: %v", err)
	}
	if open {
		t.Error("disk incident must be resolved after recovery to 0.50")
	}
	if notifier.resolvedCount() != 0 {
		t.Errorf("resolved notifications = %d, want 0 (close by saved flag, not by current window)", notifier.resolvedCount())
	}
}

func TestEvaluatorRecoveryReachesWokenChannelsAfterMaintenanceWindowEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "web-01")
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.95, time.Minute)

	asvc := alert.NewService(pool)
	chanID, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, ch, notifier)
	inWindow := true
	eval.Maint = mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil })

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick open: %v", err)
	}

	incidents := host.NewIncidentService(pool)
	in, open, err := incidents.OpenFor(ctx, h.ID, "disk")
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("disk incident must be open after 0.95 > 0.90 even in maintenance")
	}
	if !in.InMaintenance {
		t.Fatal("Incident.InMaintenance = false, want true (open must persist the flag)")
	}
	if notifier.openedCount() != 0 {
		t.Fatalf("opened notifications = %d, want 0 (open suppressed by maintenance, open-gate untouched)", notifier.openedCount())
	}

	if err := escalation.LogStep(ctx, pool, "host", in.ID, chanID, 0); err != nil {
		t.Fatalf("log step: %v", err)
	}

	inWindow = false

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedHostMetricPoint(t, ch, pid, "system.filesystem.utilization", h.Name, map[string]string{"mountpoint": "/"}, 0.50, time.Minute)

	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick resolve: %v", err)
	}

	if _, open, err := incidents.OpenFor(ctx, h.ID, "disk"); err != nil || open {
		t.Fatalf("disk incident must be resolved after recovery to 0.50 (open=%v err=%v)", open, err)
	}
	if notifier.resolvedCount() != 1 {
		t.Errorf("resolved notifications = %d, want 1 (M-7: recovery must reach woken channel despite frozen InMaintenance=true)", notifier.resolvedCount())
	}
}
