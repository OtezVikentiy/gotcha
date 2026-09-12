package slo_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
	"gitflic.ru/otezvikentiy/gotcha/internal/slo"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/uptime"
)

// Evaluator зовёт NotifyStep/NotifyRecovery, не Notify — перечитывает SLO+инцидент
// по ID тем же способом, что продовый нотифаер, чтобы проверки полей SLOEvent были верны.
type capturingNotifier struct {
	store *slo.Store

	mu     sync.Mutex
	events []slo.SLOEvent
}

func (c *capturingNotifier) Notify(_ context.Context, ev slo.SLOEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

// без возврата channelIDs мок не мог бы участвовать в цепочке «open логирует →
// close находит лог и шлёт recovery».
func (c *capturingNotifier) NotifyStep(ctx context.Context, incidentID int64, channelIDs []int64, _ int) ([]int64, error) {
	if err := c.capture(ctx, incidentID, true); err != nil {
		return nil, err
	}
	return channelIDs, nil
}

func (c *capturingNotifier) NotifyRecovery(ctx context.Context, incidentID int64, _ []int64) error {
	return c.capture(ctx, incidentID, false)
}

// калька SLOBurnNotifier.reloadEvent, только пишет в events, не в Outbox.
func (c *capturingNotifier) capture(ctx context.Context, incidentID int64, opened bool) error {
	in, ok, err := c.store.GetIncidentByID(ctx, incidentID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("capturingNotifier: incident %d not found", incidentID)
	}
	s, ok, err := c.store.Get(ctx, in.ProjectID, in.SLOID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("capturingNotifier: slo %d not found", in.SLOID)
	}
	remaining := 0.0
	if in.BudgetRemaining != nil {
		remaining = *in.BudgetRemaining
	}
	attainment := 1 - (1-remaining)*(1-s.Target)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, slo.SLOEvent{
		SLO: s, Incident: in, Opened: opened,
		Attainment: attainment, BudgetRemaining: remaining, BurnRate: in.BurnRate,
	})
	return nil
}

func (c *capturingNotifier) snapshot() []slo.SLOEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]slo.SLOEvent, len(c.events))
	copy(out, c.events)
	return out
}

// первые bad транзакций помечены сбоем.
func goodBadSpecs(n, bad int, env string) []txSpec {
	specs := make([]txSpec, 0, n)
	for i := 0; i < n; i++ {
		status := "ok"
		if i < bad {
			status = "internal_error"
		}
		specs = append(specs, txSpec{offset: time.Duration(i) * time.Millisecond, dur: 50 * time.Millisecond, status: status, env: env})
	}
	return specs
}

func TestSLOEvaluatorOpensAndCloses(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres and clickhouse containers")
	}
	pool := testenv.MigratedPG(t)
	conn := testenv.MigratedCH(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)
	// без хотя бы одного канала лесенка эскалации пуста, и notifyOpen/notifyClose
	// не находят адресата — уведомления никогда не отправятся.
	if _, err := alert.NewService(pool).CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	s, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "checkout", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, Transaction: "GET /checkout",
		BurnThreshold: 14.4, BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-10*time.Minute), goodBadSpecs(100, 20, "production"))

	notifier := &capturingNotifier{store: st}
	e := &slo.Evaluator{
		// от Interval считается бюджет тика — с дефолтом он упирается в пол 10s,
		// и на нагруженной машине запрос в CH может не уложиться.
		Interval:  time.Hour,
		Pool:      pool,
		Store:     st,
		Providers: slo.Providers(trace.NewQuery(conn), nil, nil, 90),
		Notifier:  notifier,
		Policy:    escalation.NewPolicyStore(pool),
	}

	n, err := e.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick(open): %v", err)
	}
	if n != 1 {
		t.Fatalf("переходов при открытии = %d, want 1", n)
	}
	incs, err := st.Incidents(ctx, pid, s.ID, 10)
	if err != nil || len(incs) != 1 || incs[0].Status != "open" {
		t.Fatalf("инцидент не открыт: %+v err=%v", incs, err)
	}
	evs := notifier.snapshot()
	if len(evs) != 1 || !evs[0].Opened {
		t.Fatalf("нет notify открытия: %+v", evs)
	}
	if evs[0].BurnRate < 14.4 {
		t.Fatalf("burn при открытии = %v, want >= 14.4", evs[0].BurnRate)
	}

	if n2, err := e.Tick(ctx); err != nil || n2 != 0 {
		t.Fatalf("повторный тик при открытом инциденте: переходов %d err=%v, want 0", n2, err)
	}

	seedTransactions(t, conn, pid, "GET /checkout", time.Now().UTC().Add(-1*time.Minute), goodBadSpecs(100, 0, "production"))

	for i := 0; i < 2; i++ {
		n3, err := e.Tick(ctx)
		if err != nil || n3 != 0 {
			t.Fatalf("тик %d остывания: переходов %d err=%v, want 0 (рано закрывать)", i+1, n3, err)
		}
		cur, _ := st.Incidents(ctx, pid, s.ID, 10)
		if len(cur) == 0 || cur[0].Status != "open" {
			t.Fatalf("инцидент закрыт раньше defaultCloseStreak тиков: %+v", cur)
		}
	}

	n4, err := e.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick(close): %v", err)
	}
	if n4 != 1 {
		t.Fatalf("переходов при закрытии = %d, want 1", n4)
	}
	closed, _ := st.Incidents(ctx, pid, s.ID, 10)
	if len(closed) == 0 || closed[0].Status != "resolved" {
		t.Fatalf("инцидент не закрыт после 3 тиков остывания: %+v", closed)
	}
	evs = notifier.snapshot()
	if len(evs) < 2 {
		t.Fatalf("ожидались уведомления открытия и закрытия, есть %d", len(evs))
	}
	if evs[len(evs)-1].Opened {
		t.Fatalf("последнее уведомление должно быть закрытием: %+v", evs[len(evs)-1])
	}
}

// Buckets висит до отмены ctx — имитирует голый ClickHouse-запрос без своего таймаута.
type stuckProvider struct {
	calls int32
}

func (p *stuckProvider) Buckets(ctx context.Context, _ slo.SLO, _, _ time.Time, _ time.Duration) ([]slo.Bucket, error) {
	atomic.AddInt32(&p.calls, 1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *stuckProvider) RetentionCap() time.Duration { return 0 }

func (p *stuckProvider) BucketsExcluding(ctx context.Context, s slo.SLO, from, to time.Time, step time.Duration, _ []uptime.Window) ([]slo.Bucket, error) {
	return p.Buckets(ctx, s, from, to, step)
}

func TestSLOEvaluatorTickStopsOnBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres container")
	}
	pool := testenv.MigratedPG(t)
	ctx := context.Background()
	pid := seedProject(t, pool)
	st := slo.NewStore(pool)
	if _, err := st.Create(ctx, slo.SLO{
		ProjectID: pid, Name: "stuck", Kind: slo.SLIAvailability,
		Target: 0.99, WindowDays: 30, BurnThreshold: 14.4,
		BurnLongMin: 60, BurnShortMin: 5, Enabled: true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	stuck := &stuckProvider{}
	e := &slo.Evaluator{
		Pool:      pool,
		Store:     st,
		Providers: map[slo.SLIKind]slo.Provider{slo.SLIAvailability: stuck},
		// Interval мал — бюджет тика упирается в пол (minTickBudget).
		Interval: time.Second,
	}

	started := time.Now()
	done := make(chan struct{})
	go func() {
		if _, err := e.Tick(ctx); err != nil {
			t.Errorf("Tick: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Tick не завершился: повисший провайдер блокирует оценщик")
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Errorf("Tick занял %v, want ограничение бюджетом тика (~minTickBudget)", elapsed)
	}
	if atomic.LoadInt32(&stuck.calls) == 0 {
		t.Error("оценщик не ходил в провайдер вовсе — тест не проверяет то, что должен")
	}
	if got := e.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по дедлайну тика, want 0", got)
	}
	if got := e.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
}
