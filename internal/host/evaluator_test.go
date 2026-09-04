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

// fakeNotifier — фейковая реализация host.Notifier для тестов (T12 ещё не
// написан): просто копит открытия/закрытия под мьютексом.
type fakeNotifier struct {
	mu       sync.Mutex
	opened   []host.Incident
	resolved []host.Incident

	// err — что вернуть вызывающему (модель провала постановки в outbox).
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

// NotifyStep/NotifyRecovery (B4, T7) — реролл: Evaluator больше не зовёт
// HostIncidentOpened/Resolved напрямую, а шлёт ступень лесенки/адресный
// recovery через эти методы (см. host.Evaluator.notifyOpen/notifyClose).
// Копят в те же opened/resolved слайсы, что и раньше — openedCount()/
// resolvedCount() остаются верным сигналом «нотифаер позван на открытии/
// закрытии» для существующих тестов, которым несущественно, каким именно
// методом интерфейса это случилось.
//
// NotifyStep возвращает переданные channelIDs как реально «заенкенные» (T7-
// fix): лог incident_escalations теперь пишет оркестрация (escalation.
// SendStepIfDue) по этому возврату, а не сам нотифаер — без него мок не мог
// бы участвовать в цепочке «open логирует → close находит лог и шлёт
// recovery», и resolvedCount() навсегда оставался бы нулём.
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

// seedEvalProject создаёт организацию с проектом. Каждый тест — свой проект:
// контейнер PostgreSQL переиспользуется между запусками.
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

// seedAlertChannel заводит один включённый webhook-канал проекта. Дефолт-
// лесенка эскалации (escalation.PolicyStore.Ladder, B4) резолвится из
// РЕАЛЬНЫХ enabled-каналов проекта — без единого канала её ChannelIDs пуст,
// и notifyOpen нечего логировать в incident_escalations, а notifyClose
// (RecoveryChannels) не находит адресата вовсе.
//
// Раньше (до claim-before-notify, аудит K1-1) этот хелпер был нужен только
// тестам RECOVERY (resolvedCount): NotifyStep звался независимо от того,
// есть ли что логировать, так что openedCount-тестам канал был не нужен.
// escalation.SendStepIfDue теперь при пустом ChannelIDs ступени бампит
// уровень НАПРЯМУЮ, не занимая ступень и не вызывая notifyStep вовсе (см. её
// докблок и escalation.TestSendStepIfDueBumpsWithoutNotifyWhenStepHasNoChannels)
// — канал без каналов проекта эскалация не клинит, но и не шлёт. Поэтому
// теперь seedAlertChannel нужен ЛЮБОМУ тесту, которому важно, что NotifyStep
// реально позван (в т.ч. openedCount) — без него open-уведомление тоже не
// уйдёт, и это уже не деталь реализации мока, а прямое следствие
// продуктового поведения.
func seedAlertChannel(t *testing.T, pool *pgxpool.Pool, projectID int64) {
	t.Helper()
	asvc := alert.NewService(pool)
	if _, err := asvc.CreateChannel(context.Background(), alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("seed alert channel: %v", err)
	}
}

// seedEvalHost регистрирует хост проекта и возвращает его строку.
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

// setHostLastSeen выставляет last_seen хоста напрямую — для сценария
// «молчащий хост», который store.Upsert не может смоделировать (он всегда
// ставит now()). Вместе с last_seen отодвигается и first_seen (сутки до него):
// оценщик не открывает тишину по хосту, чьё окно наблюдения короче порога
// (Evaluator.mayOpenSilent — защита от эфемерных подов), а «обычный молчащий
// сервер» по определению наблюдался долго.
func setHostLastSeen(t *testing.T, pool *pgxpool.Pool, hostID int64, lastSeen time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE hosts SET last_seen = $1, first_seen = $2 WHERE id = $3",
		lastSeen, lastSeen.Add(-24*time.Hour), hostID); err != nil {
		t.Fatalf("set last_seen: %v", err)
	}
}

// setHostFirstSeen двигает ТОЛЬКО first_seen — для сценариев вокруг окна
// наблюдения (эфемерный хост против давно живущего).
func setHostFirstSeen(t *testing.T, pool *pgxpool.Pool, hostID int64, firstSeen time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE hosts SET first_seen = $1 WHERE id = $2", firstSeen, hostID); err != nil {
		t.Fatalf("set first_seen: %v", err)
	}
}

// seedHostMetricPoint пишет точку метрики хоста в ClickHouse.
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

// newEvaluator — оценщик, который «давно работает»: StartedAt в сутках позади,
// иначе стартовый грейс (Evaluator.mayOpenSilent) не дал бы открыть тишину ни
// одному сценарию. Отдельный грейс проверяется в
// TestEvaluatorSilentGraceAfterStart.
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
		Interval:  time.Hour, // тикер не используем — дёргаем Tick вручную
		StartedAt: time.Now().UTC().Add(-24 * time.Hour),
	}
}

// TestEvaluatorDiskOpensWithWorstMountpointDetail — диск 0.95 при пороге 0.90
// (дефолт) открывает инцидент с detail худшего mountpoint, нотифаер получает
// открытие.
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

// TestEvaluatorDiskRetickBumpsSilently — повторный тик при том же нарушении
// не открывает новый инцидент и не шлёт повторное уведомление (Bump).
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

// TestEvaluatorDiskRecoveryResolves — значение упало ниже порога →
// инцидент закрывается, нотифаер получает закрытие.
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

// TestEvaluatorNoMetricDataNoIncident — метрики нет вообще → инцидент не
// открывается.
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

// TestEvaluatorMemoryRequiresUsedStateMatcher — память оценивается только по
// матчеру state=used; такое же значение с state=free порог не пробивает.
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

// TestEvaluatorLoadDividesByCoresCoresMissingSkips — load делится на число
// ядер; без метрики числа ядер нагрузка не оценивается.
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

// TestEvaluatorSilentOpensAndResolvesOnUpsert — хост, замолчавший дольше
// порога, открывает silent-инцидент; Upsert (регистрация хоста заново)
// обновляет last_seen и закрывает его.
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

// TestEvaluatorSilentResolvesInsideHysteresisDeadZone — различает «silent через
// metric.Decide с 5%-полосой» от «silent прямым сравнением» (design.md §4.4
// требует второе). Дефолтный порог 300с, полоса гистерезиса Decide была бы
// 5%×300=15с → мёртвая зона восстановления (285с;300с]. Тишина 290с внутри
// этой зоны: старая (через Decide) реализация держала бы инцидент открытым
// (Bump, не Resolve — 290 не <= 285), а прямое сравнение обязано закрыть его,
// поскольку 290 <= порог 300.
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

	// Открываем: тишина 310с — заведомо выше порога 300с и выше верхней
	// границы будущей дед-зоны.
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

	// Тишина падает до 290с — внутри мёртвой зоны гистерезиса (285с;300с], но
	// НЕ превышает порог 300с: прямое сравнение обязано резолвить.
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

// TestEvaluatorDiskDisabledSettingSkipsEvaluation — disk_enabled=false → диск
// не оценивается вообще, даже при явном нарушении.
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

// TestEvaluatorOneHostFailureDoesNotBlockNeighbor — хост без метрик (тихая
// «неудача») не мешает соседнему хосту того же тика открыть инцидент.
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

// TestEvaluatorSilentGraceAfterStart — тишина, накопленная ДО старта оценщика,
// ему не принадлежит. Продукт стоял (рестарт, недоступность PostgreSQL) —
// last_seen у всех хостов устарел, и без грейса первый же тик открыл бы silent
// РАЗОМ всем, разослав уведомление в каждый канал. Через SilentAfter после
// старта тот же хост инцидент получает: значит, грейс именно откладывает
// оценку, а не отменяет её.
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
	// Оценщик только что поднялся: наблюдает меньше порога тишины (5 минут).
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

	// Тот же хост, но оценщик наблюдает уже дольше порога — теперь молчание
	// действительно его.
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

// TestEvaluatorSilentSkipsEphemeralHost — хост, чьё окно наблюдения короче
// порога тишины (поднявшийся и умерший под), silent-инцидента не получает:
// это не «замолчавший сервер», а нормальная жизнь эфемерной машины, и каждый
// такой под иначе становился бы письмом в каждый канал проекта.
func TestEvaluatorSilentSkipsEphemeralHost(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	pid := seedEvalProject(t, pool)
	h := seedEvalHost(t, pool, pid, "pod-ephemeral")
	// Прожил минуту (порог тишины — 5) и замолчал полчаса назад.
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

	// Тот же хост, но наблюдался сутки — обычный сервер, тишина настоящая.
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

// TestEvaluatorSilentDoesNotBumpEveryTick — повторный тик по тому же
// молчащему хосту не переписывает строку инцидента. Проверяется по xmin
// (системный столбец PostgreSQL, меняющийся на КАЖДОМ UPDATE) — сравнение
// значений поймало бы только запись другого числа, а цена как раз в самом
// UPDATE: 1440 записей в сутки на хост ради производной величины.
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

	// Тишина выросла в разы — вот теперь обновить peak имеет смысл.
	setHostLastSeen(t, pool, h.ID, time.Now().UTC().Add(-60*time.Minute))
	if err := eval.Tick(ctx); err != nil {
		t.Fatalf("Tick 3: %v", err)
	}
	if after := xmin(); after == before {
		t.Error("шестикратный рост тишины не обновил инцидент — peak перестал отражать максимум")
	}
}

// stuckCH — ClickHouse, который «висит»: любой запрос блокируется до отмены
// контекста. Ровно так выглядит недоступная база для драйвера с ReadTimeout в
// 300 секунд. Все прочие методы наследуются от вложенного nil-интерфейса —
// оценщик их не зовёт, а вызов упал бы паникой и был бы виден.
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

// TestEvaluatorSilentEvaluatedWhileClickHouseHangs — главный сценарий отказа:
// ClickHouse не отвечает. Тишина считается по одному PostgreSQL и обязана быть
// оценена ДО первого похода в CH — иначе продукт перестаёт сообщать «сервер
// лёг» ровно тогда, когда это нужнее всего. Заодно проверяется, что тик
// заканчивается по дедлайну, а не висит вместе с CH.
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
	seedEvalHost(t, pool, pid, "noisy-neighbour") // живой сосед — его пороги и полезут в CH

	stuck := &stuckCH{}
	notifier := &fakeNotifier{}
	eval := newEvaluator(pool, nil, notifier)
	eval.Metrics = metric.NewQuery(stuck)
	// Бюджет тика упирается в пол (minTickBudget), поэтому тик завершится
	// примерно через него, а не через долю от Interval.
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
	// Тик вышел по дедлайну — отметку «последний завершённый проход» он
	// публиковать не должен: иначе оценщик, каждый раз обрывающийся на
	// половине парка, снаружи выглядел бы идеально здоровым.
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

// countingCH считает запросы типа метрики (any(type) — тело metricType),
// пропуская всё остальное в настоящий ClickHouse.
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

// TestEvaluatorCachesMetricTypePerTick — тип метрики не зависит от хоста,
// поэтому у проекта с несколькими машинами одинаковые запросы «какого типа
// system.filesystem.utilization» повторялись на каждый хост. Кеш на тик: два
// хоста одного проекта — один запрос типа на метрику.
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

	// Четыре метрики × один запрос типа на метрику, независимо от числа хостов.
	if got := counting.typeQueries.Load(); got != 4 {
		t.Errorf("запросов типа метрики = %d, want 4 (по одному на метрику на весь тик, а не на каждый хост)", got)
	}
}

// TestEvaluatorPublishesTickLiveness — self-метрики живости: после успешного
// тика оценщик знает, когда тот закончился и сколько шёл. Без этих чисел
// «оценщик умер» снаружи неотличимо от «на хостах спокойно»: молчание — его
// нормальный вывод.
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

// cancellingNotifier отменяет контекст тика ровно в момент, когда его зовут на
// открытии инцидента, — модель «бюджет тика кончился посреди
// последовательности „инцидент открыт → уведомить → пометить отправленным“».
// Заодно запоминает, живым ли контекст пришёл ему самому: реальный
// HostNotifier по этому контексту ставит задачу в outbox.
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

// TestEvaluatorNotifiesEvenWhenTickBudgetRunsOut — уведомление и его пометка
// отвязаны от дедлайна тика. У подсистемы хостов нет досылки по флагу (в
// отличие от uptime с last_reminded_at), поэтому уведомление, не поставленное
// в очередь из-за исчерпанного бюджета, не ушло бы НИКОГДА: инцидент открыт,
// notified_open=false, и «сервер лёг» не узнал бы никто.
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

	// Главное: пометка отправки дошла до базы, несмотря на отменённый контекст
	// тика. Без неё инцидент навсегда остался бы «открыт, но не уведомлён».
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

// TestEvaluatorKeepsNotifiedFalseWhenNotifierFails — провал нотификатора
// (постановка в outbox не удалась) НЕ помечает инцидент уведомлённым. Флаг
// notified_open читает только человек, разбирающий «почему не пришло письмо»:
// проставленный после провала, он отправляет его искать проблему на своей
// стороне — там, где её нет.
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

	// Инцидент открыт (провал уведомления не отменяет самого инцидента), и
	// нотификатор был позван — иначе тест проверял бы не то.
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

// TestEvaluatorHostOverrideOpensBelowProjectThreshold — Task 5: оценщик
// берёт ЭФФЕКТИВНЫЙ порог из каскада (host-override → group → project →
// default), а не проектный напрямую. Per-host override диска (0.50) должен
// открыть инцидент при утилизации 60%, хотя дефолтный проектный порог
// (0.90) при тех же 60% инцидент бы не открыл.
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

// TestEvaluatorDisablingViaOverrideResolvesOpenIncident — M-A (брифа Task
// 5): host-override, выключивший вид, у которого уже есть открытый
// инцидент, обязан этот инцидент закрыть на следующем тике — иначе он висел
// бы открытым вечно (ручного закрытия инцидента хоста в интерфейсе нет).
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

// TestEvaluatorGroupThresholdOpensBelowProjectThreshold — QA P2-2 ремедиации:
// СОХРАНЁННОЕ групповое правило (role/env) реально меняет оценку через Tick,
// а не только резолвер в изоляции (resolve_test.go проверяет
// ThresholdResolver.Effective без БД/оценщика). Хост с role="web", групповое
// правило role/web disk_threshold=0.50 — открывает инцидент при диске 60%,
// хотя проектный порог (дефолт 0.90) при такой утилизации не открыл бы
// ничего.
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

// TestEvaluatorDisablingViaOverrideResolvesOpenSilentIncident — M-A (брифа
// Task 5), silent-ветка ремедиации QA P2-3:
// TestEvaluatorDisablingViaOverrideResolvesOpenIncident покрывает только
// disk, а evalOrCloseKind гейтит ВСЕ четыре вида, включая silent (первый
// проход Tick) — silent_enabled=false через host-override при уже открытом
// silent-инциденте обязан закрыть его на следующем тике.
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

// mockMaint — host.MaintenanceChecker для тестов: func-обёртка вместо
// полноценного uptime.Service (интерфейс здесь в один метод — реальный
// сервис с окнами обслуживания и своей БД тестам этого пакета не нужен).
type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

// TestEvaluatorMaintenanceSuppressesThresholdNotify — MAJOR-3 брифа Task 3:
// пороговый сайт (applyDecision, disk/memory/load). Открытие в окне
// обслуживания пишет инцидент в БД с in_maintenance=true, но НЕ уведомляет;
// закрытие того же инцидента (ещё внутри окна) тоже не уведомляет.
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

// TestEvaluatorMaintenanceFalseStillNotifies — Maint заполнен (не nil), но
// вне окна (InMaintenance→false): поведение обычное, уведомление уходит.
// Отличает «MaintenanceChecker сконфигурирован и говорит false» от
// «MaintenanceChecker==nil» (последнее уже покрыто остальными тестами файла
// back-compat'ом — см. бриф Task 3, nil-guard).
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

// TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds — дискриминирует
// close-гейт «по сохранённому флагу инцидента» (!open.InMaintenance) от
// ошибочного «по текущему окну» (!e.inMaintenance(now)): открываем инцидент В
// окне (in_maintenance=true), затем окно ЗАКАНЧИВАЕТСЯ (mock переключается на
// false) — close всё равно должен быть подавлен, т.к. читается сохранённый
// флаг инцидента, а не текущее состояние окна. Перепиши close-гейт на «сейчас
// окно» — при mock→false close разуведомит, и этот тест упадёт.
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

	// Окно обслуживания закончилось — mock переключаем на false. Close-гейт
	// обязан смотреть на сохранённый open.InMaintenance (true), а не на
	// текущее состояние окна.
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

// TestEvaluatorRecoveryReachesWokenChannelsAfterMaintenanceWindowEnds — M-7
// (аудит B4, remediation A): инцидент открыт В окне обслуживания
// (in_maintenance заморожен=true на инциденте, open-гейт не тронут — открытие
// молчит), но за время жизни инцидента эскалация реально разбудила канал
// (планировщик T8, здесь симулируем логом эскалации напрямую — вне этого
// пакета). Окно кончается, инцидент резолвится ВНЕ окна: close ОБЯЗАН
// прислать recovery разбуженному каналу, несмотря на замороженный
// InMaintenance=true — старый гейт `!open.InMaintenance` на close-пути гасил
// именно этот случай (M-7). Дискриминирует: падает, если гейт вернуть.
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

	// Планировщик (T8, вне этого пакета) реально эскалировал инцидент после
	// открытия — разбудил канал. Симулируем это логом эскалации напрямую, как
	// советует бриф remediation A, не поднимая живой планировщик в тесте.
	if err := escalation.LogStep(ctx, pool, "host", in.ID, chanID, 0); err != nil {
		t.Fatalf("log step: %v", err)
	}

	// Окно обслуживания закончилось.
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
