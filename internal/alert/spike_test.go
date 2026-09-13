package alert_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/alert"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// Опрашивает outbox до достижения want или дедлайна — годится и для
// доказательства присутствия, и отсутствия (по итоговому счётчику).
func waitOutboxPending(t *testing.T, pool *pgxpool.Pool, want int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var n int
	for {
		if err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM notification_outbox WHERE status = 'pending'").Scan(&n); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		if n >= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSpikeDetectsThresholdBreach(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := alert.NewService(pool)
	issueSvc := issue.NewService(pool)
	eventQuery := event.NewQuery(ch)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newEvalProject(t, pool, "spikeA")
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		// 30, не 0 — условие спайка держится всю жизнь теста, и без троттлинга
		// каждый 20-мс тик после первого дал бы дубль.
		ProjectID: pid, Kind: alert.KindSpike, Enabled: true, Threshold: 3, WindowMinutes: 10, ThrottleMinutes: 30,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	res, err := issueSvc.Upsert(ctx, pid, "fp-spike", "spiking issue", "app.x", "error", "", time.Now())
	if err != nil {
		t.Fatalf("issue upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		b.Add(event.Event{
			ID:        uuid.NewString(),
			ProjectID: pid,
			IssueID:   res.IssueID,
			Timestamp: now.Add(-time.Duration(i) * time.Minute),
			Level:     "error",
			Message:   "boom",
		})
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("batcher close: %v", err)
	}

	e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
	sp := &alert.Spike{Svc: svc, Outbox: ob, Issues: issueSvc, Events: eventQuery, Evaluator: e, Interval: 20 * time.Millisecond}

	spCtx, spCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sp.Run(spCtx)
		close(done)
	}()

	n := waitOutboxPending(t, pool, 1, 5*time.Second)
	spCancel()
	<-done

	if n != 1 {
		t.Fatalf("outbox pending = %d, want 1 (spike threshold breached)", n)
	}

	jobs, err := ob.Claim(ctx, 10)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("claimed jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Payload["kind"] != alert.KindSpike {
		t.Errorf("payload kind = %v, want spike", jobs[0].Payload["kind"])
	}
	if jobs[0].Payload["issue_id"] != float64(res.IssueID) {
		t.Errorf("payload issue_id = %v, want %d", jobs[0].Payload["issue_id"], res.IssueID)
	}
}

func TestSpikeBelowThresholdSendsNothing(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := alert.NewService(pool)
	issueSvc := issue.NewService(pool)
	eventQuery := event.NewQuery(ch)
	ob := notify.NewOutbox(pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pid := newEvalProject(t, pool, "spikeB")
	if _, err := svc.CreateChannel(ctx, alert.Channel{
		ProjectID: pid, Kind: alert.ChannelWebhook, Enabled: true, Target: "https://example.com/hook",
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := svc.UpsertRule(ctx, alert.Rule{
		// 30, не 0 — условие спайка держится всю жизнь теста, и без троттлинга
		// каждый 20-мс тик после первого дал бы дубль.
		ProjectID: pid, Kind: alert.KindSpike, Enabled: true, Threshold: 3, WindowMinutes: 10, ThrottleMinutes: 30,
	}); err != nil {
		t.Fatalf("UpsertRule: %v", err)
	}

	res, err := issueSvc.Upsert(ctx, pid, "fp-spike", "spiking issue", "app.x", "error", "", time.Now())
	if err != nil {
		t.Fatalf("issue upsert: %v", err)
	}

	b := event.NewBatcher(ch)
	go b.Run()
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		b.Add(event.Event{
			ID:        uuid.NewString(),
			ProjectID: pid,
			IssueID:   res.IssueID,
			Timestamp: now.Add(-time.Duration(i) * time.Minute),
			Level:     "error",
			Message:   "boom",
		})
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("batcher close: %v", err)
	}

	e := &alert.Evaluator{Svc: svc, Outbox: ob, BaseURL: "https://gotcha.example"}
	sp := &alert.Spike{Svc: svc, Outbox: ob, Issues: issueSvc, Events: eventQuery, Evaluator: e, Interval: 20 * time.Millisecond}

	spCtx, spCancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sp.Run(spCtx)
		close(done)
	}()

	n := waitOutboxPending(t, pool, 1, 300*time.Millisecond)
	spCancel()
	<-done

	if n != 0 {
		t.Fatalf("outbox pending = %d, want 0 (below threshold)", n)
	}
}

func TestSpikePublishesTickLiveness(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	sp := &alert.Spike{Svc: svc, Interval: time.Hour}

	if got := sp.LastTickUnix(); got != 0 {
		t.Fatalf("LastTickUnix до первого тика = %d, want 0", got)
	}

	before := time.Now().Unix()
	sp.Tick(context.Background())

	if got := sp.LastTickUnix(); got < before {
		t.Errorf("LastTickUnix = %d, want >= %d (момент завершения тика)", got, before)
	}
	if got := sp.LastTickSeconds(); got < 0 || got > 5 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность в разумных пределах", got)
	}
}

// Без context.WithTimeout повисший SpikeRules держал бы цикл спайков
// бесконечно вместо прерывания по бюджету (пол 10с).
func TestSpikeTickBudgetAbortsHungTick(t *testing.T) {
	pool := testenv.MigratedPG(t)
	svc := alert.NewService(pool)
	sp := &alert.Spike{Svc: svc, Interval: time.Second}

	lockConn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lockConn.Release()
	tx, err := lockConn.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(), "LOCK TABLE alert_rules IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock alert_rules: %v", err)
	}
	defer tx.Rollback(context.Background())

	started := time.Now()
	done := make(chan struct{})
	go func() {
		sp.Tick(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Tick не вернулся за 20s — SpikeRules не ограничен бюджетом")
	}
	if dur := time.Since(started); dur > 15*time.Second {
		t.Errorf("Tick вернулся через %v, want ограничение бюджетом (пол 10с)", dur)
	}
	if got := sp.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по бюджету тика, want 0", got)
	}
	if got := sp.LastTickSeconds(); got <= 0 {
		t.Errorf("LastTickSeconds = %v, want положительную длительность даже у оборванного тика", got)
	}
}

// Блокирует Issues.ByIDs внутри цикла, не SpikeRules (как в
// TestSpikeTickBudgetAbortsHungTick) — иначе ctx.Err() перед вторым правилом остался бы непроверенным.
func TestSpikeTickBudgetSkipsRemainingRules(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	svc := alert.NewService(pool)
	issueSvc := issue.NewService(pool)
	eventQuery := event.NewQuery(ch)
	ctx := context.Background()

	b := event.NewBatcher(ch)
	go b.Run()
	now := time.Now().UTC()

	for i := 0; i < 2; i++ {
		pid := newEvalProject(t, pool, fmt.Sprintf("spike-budget-%d", i))
		if _, err := svc.UpsertRule(ctx, alert.Rule{
			ProjectID: pid, Kind: alert.KindSpike, Enabled: true, Threshold: 3, WindowMinutes: 10, ThrottleMinutes: 30,
		}); err != nil {
			t.Fatalf("UpsertRule: %v", err)
		}
		res, err := issueSvc.Upsert(ctx, pid, "fp-spike-budget", "spiking issue", "app.x", "error", "", now)
		if err != nil {
			t.Fatalf("issue upsert: %v", err)
		}
		for j := 0; j < 4; j++ {
			b.Add(event.Event{
				ID:        uuid.NewString(),
				ProjectID: pid,
				IssueID:   res.IssueID,
				Timestamp: now.Add(-time.Duration(j) * time.Minute),
				Level:     "error",
				Message:   "boom",
			})
		}
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("batcher close: %v", err)
	}

	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lockConn.Release()
	tx, err := lockConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE issues IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock issues: %v", err)
	}
	defer tx.Rollback(ctx)

	e := &alert.Evaluator{Svc: svc, Outbox: notify.NewOutbox(pool), BaseURL: "https://gotcha.example"}
	sp := &alert.Spike{Svc: svc, Issues: issueSvc, Events: eventQuery, Evaluator: e, Interval: time.Second}

	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	started := time.Now()
	done := make(chan struct{})
	go func() {
		sp.Tick(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Tick не вернулся за 20s — Issues.ByIDs не ограничен бюджетом")
	}
	if dur := time.Since(started); dur > 15*time.Second {
		t.Errorf("Tick вернулся через %v, want ограничение бюджетом (пол 10с)", dur)
	}

	if got := sp.LastTickSkippedRules(); got != 1 {
		t.Errorf("LastTickSkippedRules() = %d, want 1 (второе правило пропущено по бюджету)", got)
	}
	if got := sp.LastTickUnix(); got != 0 {
		t.Errorf("LastTickUnix = %d после оборванного по бюджету тика, want 0", got)
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "tick budget exhausted, remaining rules skipped") {
		t.Errorf("лог не содержит явного пропуска по бюджету: %s", logs)
	}
}
