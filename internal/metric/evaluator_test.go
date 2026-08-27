package metric_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

func seedMetricGauge(t *testing.T, ch driver.Conn, projectID int64, name string, val float64, ago time.Duration) {
	t.Helper()
	if err := ch.Exec(context.Background(), `
		INSERT INTO metric_points (project_id, name, type, unit, service, environment, attributes, ts, value, count, bucket_counts, explicit_bounds, monotonic, temporality)
		VALUES (?, ?, 'gauge', '1', 'api', '', map(), ?, ?, 0, [], [], 0, '')`,
		projectID, name, time.Now().UTC().Add(-ago), val); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}
}

func TestEvaluatorOpenCloseAlertOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}

	eval := &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Incidents: incidents, Rules: rules, Pool: pool},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour, // тикер не используем — дёргаем Tick вручную
	}

	// Среднее 150 > 100 → инцидент открыт, одно уведомление.
	seedMetricGauge(t, ch, projectID, "cpu", 140, time.Minute)
	seedMetricGauge(t, ch, projectID, "cpu", 160, 2*time.Minute)
	eval.Tick(ctx)

	if _, open, _ := incidents.OpenFor(ctx, rule.ID); !open {
		t.Fatalf("incident must be open after breach")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("open jobs = %d, want 1", len(jobs))
	}

	// Повторный тик при тех же данных → инцидент открыт, НОВЫХ уведомлений нет.
	eval.Tick(ctx)
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 0 {
		t.Fatalf("re-tick produced %d new jobs, want 0 (alert once)", len(jobs2))
	}

	// Данные упали ниже порога (окно теперь содержит только низкие значения) →
	// инцидент закрыт, одно уведомление о закрытии.
	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedMetricGauge(t, ch, projectID, "cpu", 50, time.Minute)
	eval.Tick(ctx)

	if _, open, _ := incidents.OpenFor(ctx, rule.ID); open {
		t.Fatalf("incident must be resolved after recovery")
	}
	jobs3, _ := ob.Claim(ctx, 10)
	if len(jobs3) != 1 {
		t.Fatalf("close jobs = %d, want 1", len(jobs3))
	}
}

// mockMaint — metric.MaintenanceChecker для тестов: func-обёртка вместо
// полноценного uptime.Service (интерфейс здесь в один метод — реальный
// сервис с окнами обслуживания и своей БД тестам этого пакета не нужен).
// Зеркало host.mockMaint (internal/host/evaluator_test.go).
type mockMaint func(ctx context.Context, projectID int64, at time.Time) (bool, error)

func (m mockMaint) InMaintenance(ctx context.Context, projectID int64, at time.Time) (bool, error) {
	return m(ctx, projectID, at)
}

// TestEvaluatorMaintenanceSuppressesNotify — B3: открытие инцидента в окне
// обслуживания пишет инцидент в БД с in_maintenance=true, но НЕ уведомляет;
// закрытие того же инцидента (ещё внутри окна) тоже не уведомляет. Зеркало
// host.TestEvaluatorMaintenanceSuppressesThresholdNotify.
func TestEvaluatorMaintenanceSuppressesNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}

	eval := &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Incidents: incidents, Rules: rules, Pool: pool},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour,
		Maint:    mockMaint(func(context.Context, int64, time.Time) (bool, error) { return true, nil }),
	}

	seedMetricGauge(t, ch, projectID, "cpu", 140, time.Minute)
	seedMetricGauge(t, ch, projectID, "cpu", 160, 2*time.Minute)
	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("incident must be open after breach even in maintenance")
	}
	if !in.InMaintenance {
		t.Error("Incident.InMaintenance = false, want true")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 0 {
		t.Errorf("open jobs = %d, want 0 (suppressed by maintenance)", len(jobs))
	}

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedMetricGauge(t, ch, projectID, "cpu", 50, time.Minute)
	eval.Tick(ctx)

	if _, open, _ := incidents.OpenFor(ctx, rule.ID); open {
		t.Error("incident must be resolved after recovery")
	}
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 0 {
		t.Errorf("close jobs = %d, want 0 (suppressed by maintenance)", len(jobs2))
	}
}

// TestEvaluatorMaintenanceFalseStillNotifies — Maint заполнен (не nil), но
// вне окна (InMaintenance→false): поведение обычное, уведомление уходит.
// Отличает «MaintenanceChecker сконфигурирован и говорит false» от
// «MaintenanceChecker==nil» (последнее уже покрыто TestEvaluatorOpenCloseAlertOnce).
// Зеркало host.TestEvaluatorMaintenanceFalseStillNotifies.
func TestEvaluatorMaintenanceFalseStillNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}

	eval := &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Incidents: incidents, Rules: rules, Pool: pool},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour,
		Maint:    mockMaint(func(context.Context, int64, time.Time) (bool, error) { return false, nil }),
	}

	seedMetricGauge(t, ch, projectID, "cpu", 140, time.Minute)
	seedMetricGauge(t, ch, projectID, "cpu", 160, 2*time.Minute)
	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("incident must be open after breach")
	}
	if in.InMaintenance {
		t.Error("Incident.InMaintenance = true, want false (outside window)")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Errorf("open jobs = %d, want 1 (not suppressed outside maintenance)", len(jobs))
	}
}

// TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds — дискриминирует
// close-гейт «по сохранённому флагу инцидента» (!open.InMaintenance) от
// ошибочного «по текущему окну» (!e.inMaintenance(now)): открываем инцидент В
// окне, затем окно ЗАКАНЧИВАЕТСЯ (mock→false) — close всё равно должен быть
// подавлен, т.к. читается сохранённый флаг инцидента. Зеркало
// host.TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds.
func TestEvaluatorMaintenanceCloseSuppressedByFlagAfterWindowEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	rules := metric.NewRuleService(pool)
	incidents := metric.NewIncidentService(pool)
	projectID := seedProject(t, pool)

	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: projectID, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}
	rule, err := rules.Create(ctx, metric.Rule{
		ProjectID: projectID, MetricName: "cpu", Aggregation: "avg", Comparator: "gt",
		Threshold: 100, WindowSeconds: 300, Enabled: true,
	})
	if err != nil {
		t.Fatalf("rule: %v", err)
	}

	inWindow := true
	eval := &metric.Evaluator{
		Rules: rules, Query: metric.NewQuery(ch), Incidents: incidents,
		Notifier: &metric.MetricNotifier{Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example", Incidents: incidents, Rules: rules, Pool: pool},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour,
		Maint:    mockMaint(func(context.Context, int64, time.Time) (bool, error) { return inWindow, nil }),
	}

	seedMetricGauge(t, ch, projectID, "cpu", 140, time.Minute)
	seedMetricGauge(t, ch, projectID, "cpu", 160, 2*time.Minute)
	eval.Tick(ctx)

	in, open, err := incidents.OpenFor(ctx, rule.ID)
	if err != nil {
		t.Fatalf("OpenFor: %v", err)
	}
	if !open {
		t.Fatal("incident must be open after breach even in maintenance")
	}
	if !in.InMaintenance {
		t.Fatal("Incident.InMaintenance = false, want true")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 0 {
		t.Fatalf("open jobs = %d, want 0 (suppressed by maintenance)", len(jobs))
	}

	// Окно закончилось — close-гейт должен смотреть на сохранённый флаг
	// инцидента, а не на текущее состояние окна.
	inWindow = false

	if err := ch.Exec(ctx, "TRUNCATE TABLE metric_points"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedMetricGauge(t, ch, projectID, "cpu", 50, time.Minute)
	eval.Tick(ctx)

	if _, open, _ := incidents.OpenFor(ctx, rule.ID); open {
		t.Error("incident must be resolved after recovery")
	}
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 0 {
		t.Errorf("close jobs = %d, want 0 (close by saved flag, not by current window)", len(jobs2))
	}
}

// fakeRuleLister — минимальная реализация ruleLister для тестов, которым не
// нужны контейнеры: Tick требует только эту зависимость до первого evalRule.
type fakeRuleLister struct {
	rules []metric.Rule
	err   error
	// block, если true, держит ListEnabled до отмены ctx — модель повисшего
	// ClickHouse/PostgreSQL похода без реальной инфраструктуры.
	block bool
	calls atomic.Int64
}

func (f *fakeRuleLister) ListEnabled(ctx context.Context) ([]metric.Rule, error) {
	f.calls.Add(1)
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.rules, f.err
}

// TestEvaluatorPublishesTickLiveness — self-метрики живости: без них умерший
// или отставший metric.Evaluator снаружи неотличим от «правил, готовых
// сработать, сейчас нет».
func TestEvaluatorPublishesTickLiveness(t *testing.T) {
	rules := &fakeRuleLister{}
	eval := &metric.Evaluator{Rules: rules, Interval: time.Hour}

	if got := eval.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	before := time.Now().Unix()
	eval.Tick(context.Background())

	if got := eval.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := eval.LastTickSeconds(); got <= 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

// TestEvaluatorTickBudgetAbortsHungTick — повисший ListEnabled (голый
// ClickHouse/PostgreSQL запрос без своего таймаута) не должен блокировать тик
// дольше бюджета: тот же контракт, что host.Evaluator.
func TestEvaluatorTickBudgetAbortsHungTick(t *testing.T) {
	rules := &fakeRuleLister{block: true}
	// Interval мал — бюджет тика упирается в пол (minTickBudget), как у
	// host.Evaluator в его аналогичном тесте.
	eval := &metric.Evaluator{Rules: rules, Interval: time.Second}

	done := make(chan struct{})
	go func() {
		eval.Tick(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Tick не завершился: повисший источник правил блокирует оценщик")
	}

	if rules.calls.Load() == 0 {
		t.Error("оценщик не звал ListEnabled вовсе — тест не проверяет то, что должен")
	}
	// Тик вышел по дедлайну — отметку «последний завершённый проход» он
	// публиковать не должен, иначе постоянно обрывающийся тик снаружи выглядел
	// бы здоровым.
	if got := eval.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по дедлайну тика, want 0", got)
	}
	if got := eval.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
}
