package profile_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// seedProfSample сеет v строк функции leaf весом 1 каждая (не одну строку
// весом v): MinSamples гейтит число строк окна, а не сумму их веса, поэтому
// вызывающие тесты, читающие v как «v сэмплов», обязаны реально получить v
// строк. Сумма весов группы (self) от этого не меняется — она равна v что
// при старой, что при новой раскладке, — поэтому доля (Share) во всех местах
// вызова остаётся прежней.
func seedProfSample(t *testing.T, ch driver.Conn, projectID int64, leaf string, v uint64, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	ts := time.Now().UTC().Add(-ago)
	batch, err := ch.PrepareBatch(ctx, `INSERT INTO profile_samples
		(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)`)
	if err != nil {
		t.Fatalf("seed prepare batch: %v", err)
	}
	stack := []string{"root", leaf}
	for i := uint64(0); i < v; i++ {
		if err := batch.Append(uint64(projectID), "cpu", "api", "", "", "go", ts, stack, uint64(1), ""); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	if err := batch.Send(); err != nil {
		t.Fatalf("seed send: %v", err)
	}
}

// countingQuery считает обращения цикла к ClickHouse — позитивный контроль,
// которого тесту не хватало.
type countingQuery struct {
	mu    sync.Mutex
	calls int
}

func (c *countingQuery) ActiveServices(context.Context, time.Time, time.Time) ([]profile.ProjectService, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return nil, errNoServices
}

func (c *countingQuery) TopFunctionShares(context.Context, int64, string, string, time.Time, time.Time, int) ([]profile.FunctionShare, error) {
	return nil, nil
}

func (c *countingQuery) BaselineFunctionShares(context.Context, int64, string, string, []string, int, time.Time) (map[string]profile.BaselineShare, error) {
	return nil, nil
}

func (c *countingQuery) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var errNoServices = errors.New("no services")

// TestRegressionEvaluatorRun: цикл обязан РЕАЛЬНО тикать и завершаться по
// отмене контекста.
//
// Раньше единственным утверждением было «горутина вышла после cancel» — тест
// оставался зелёным и с вырезанным вызовом Tick, потому что проверял
// завершение цикла, а не его работу.
func TestRegressionEvaluatorRun(t *testing.T) {
	queries := &countingQuery{}
	eval := &profile.RegressionEvaluator{
		Query:    queries,
		Interval: 2 * time.Millisecond,
		Config:   profile.DefaultProfileRegressionConfig(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eval.Run(ctx); close(done) }()

	deadline := time.After(5 * time.Second)
	for queries.count() == 0 {
		select {
		case <-deadline:
			cancel()
			t.Fatal("цикл не обратился к списку сервисов ни разу — Tick не выполняется")
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run не завершился после отмены контекста")
	}

	// Interval<=0 → используется evaluatorDefaultInterval (5m): тикер не успеет
	// сработать, но ветка выбора интервала выполняется; отмена завершает цикл.
	eval.Interval = 0
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { eval.Run(ctx2); close(done2) }()
	cancel2()
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("Run с дефолтным интервалом не завершился после отмены контекста")
	}
}

func TestRegressionEvaluatorOpenCloseAlertOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("requires containers")
	}
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	asvc := alert.NewService(pool)
	ob := notify.NewOutbox(pool)
	pid := seedProject(t, pool)
	if _, err := asvc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("channel: %v", err)
	}

	cfg := profile.DefaultProfileRegressionConfig() // Threshold 0.5, MinSamples 100, ShareFloor 0.05, BaselineDays 7
	regressions := profile.NewRegressionService(pool)
	eval := &profile.RegressionEvaluator{
		Query: profile.NewQuery(ch), Regressions: regressions,
		Notifier: &profile.RegressionNotifier{
			Alerts: asvc, Outbox: ob, BaseURL: "https://gotcha.example",
			Regressions: regressions, Pool: pool,
		},
		Policy:   escalation.NewPolicyStore(pool),
		Pool:     pool,
		Interval: time.Hour, Config: cfg,
	}

	// Свежее окно: slow — 80 из 100 (80%). Прошлые дни: slow — 30 из 300 (10%) →
	// база (медиана) ~0.1 → рост +700% ≥ порога, доля ≥ пола, samples=100 → Open.
	seedProfSample(t, ch, pid, "slow", 80, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 20, 5*time.Minute)
	seedProfSample(t, ch, pid, "slow", 30, 24*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 24*time.Hour)
	seedProfSample(t, ch, pid, "slow", 30, 48*time.Hour)
	seedProfSample(t, ch, pid, "other", 270, 48*time.Hour)

	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); !open {
		t.Fatalf("regression must be open after breach")
	}
	jobs, _ := ob.Claim(ctx, 10)
	if len(jobs) != 1 {
		t.Fatalf("open jobs = %d, want 1", len(jobs))
	}

	// Повторный тик — новых уведомлений нет (алерт один раз).
	eval.Tick(ctx)
	jobs2, _ := ob.Claim(ctx, 10)
	if len(jobs2) != 0 {
		t.Fatalf("re-tick produced %d jobs, want 0", len(jobs2))
	}

	// Доля упала: очищаем и сеем низкую свежую долю → инцидент закрыт, одно
	// уведомление о закрытии.
	if err := ch.Exec(ctx, "TRUNCATE TABLE profile_samples"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedProfSample(t, ch, pid, "slow", 5, 5*time.Minute)
	seedProfSample(t, ch, pid, "other", 95, 5*time.Minute)
	eval.Tick(ctx)
	if _, open, _ := eval.Regressions.OpenFor(ctx, pid, "api", "cpu", "slow"); open {
		t.Fatalf("regression must be resolved after recovery")
	}
	jobs3, _ := ob.Claim(ctx, 10)
	if len(jobs3) != 1 {
		t.Fatalf("close jobs = %d, want 1", len(jobs3))
	}
}

// emptyQuery — profileQuery, чей ActiveServices успешно возвращает пустой
// список: тику незачем идти дальше, но он обязан ЗАВЕРШИТЬСЯ успешно, в
// отличие от countingQuery (та нарочно возвращает ошибку).
type emptyQuery struct{}

func (emptyQuery) ActiveServices(context.Context, time.Time, time.Time) ([]profile.ProjectService, error) {
	return nil, nil
}

func (emptyQuery) TopFunctionShares(context.Context, int64, string, string, time.Time, time.Time, int) ([]profile.FunctionShare, error) {
	return nil, nil
}

func (emptyQuery) BaselineFunctionShares(context.Context, int64, string, string, []string, int, time.Time) (map[string]profile.BaselineShare, error) {
	return nil, nil
}

// blockingQuery — profileQuery, чей ActiveServices держит вызов до отмены
// ctx: модель повисшего ClickHouse-похода без реальной инфраструктуры.
type blockingQuery struct {
	calls atomic.Int64
}

func (b *blockingQuery) ActiveServices(ctx context.Context, _, _ time.Time) ([]profile.ProjectService, error) {
	b.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingQuery) TopFunctionShares(context.Context, int64, string, string, time.Time, time.Time, int) ([]profile.FunctionShare, error) {
	return nil, nil
}

func (b *blockingQuery) BaselineFunctionShares(context.Context, int64, string, string, []string, int, time.Time) (map[string]profile.BaselineShare, error) {
	return nil, nil
}

// TestRegressionEvaluatorPublishesTickLiveness — self-метрики живости: без
// них умерший или отставший RegressionEvaluator снаружи неотличим от
// «регрессий профилей нет».
func TestRegressionEvaluatorPublishesTickLiveness(t *testing.T) {
	eval := &profile.RegressionEvaluator{Query: &emptyQuery{}, Interval: time.Hour}
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

// TestRegressionEvaluatorTickBudgetAbortsHungTick — повисший ActiveServices
// (голый ClickHouse-запрос без своего таймаута) не должен блокировать тик
// дольше бюджета: тот же контракт, что host.Evaluator/metric.Evaluator.
func TestRegressionEvaluatorTickBudgetAbortsHungTick(t *testing.T) {
	q := &blockingQuery{}
	eval := &profile.RegressionEvaluator{Query: q, Interval: time.Second}

	done := make(chan struct{})
	go func() {
		eval.Tick(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Tick не завершился: повисший ClickHouse блокирует оценщик")
	}

	if q.calls.Load() == 0 {
		t.Error("оценщик не звал ActiveServices вовсе — тест не проверяет то, что должен")
	}
	if got := eval.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по дедлайну тика, want 0", got)
	}
	if got := eval.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
}
