package alert_test

import (
	"context"
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

// waitOutboxPending polls notification_outbox for a pending count >= want,
// returning early once satisfied, or leaves the caller to observe the final
// count once the deadline elapses (used both to prove presence and absence).
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
		// ThrottleMinutes=30 (not 0/"no throttle"): the spike condition stays
		// true for the rest of the test's short lifetime, and Spike ticks
		// every 20ms, so without a throttle window every tick after the
		// first would enqueue another duplicate job.
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
		// ThrottleMinutes=30 (not 0/"no throttle"): the spike condition stays
		// true for the rest of the test's short lifetime, and Spike ticks
		// every 20ms, so without a throttle window every tick after the
		// first would enqueue another duplicate job.
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

	// Give several ticks a chance to run, then confirm nothing was enqueued.
	n := waitOutboxPending(t, pool, 1, 300*time.Millisecond)
	spCancel()
	<-done

	if n != 0 {
		t.Fatalf("outbox pending = %d, want 0 (below threshold)", n)
	}
}
