package notify_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/notify"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// fakeSender records calls and either always succeeds or always fails.
type fakeSender struct {
	failAlways bool
	calls      int32
	mu         sync.Mutex
	targets    []notify.Target
}

func (f *fakeSender) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.targets = append(f.targets, t)
	f.mu.Unlock()
	if f.failAlways {
		return errors.New("fake send failure")
	}
	return nil
}

func enqueueJob(t *testing.T, ob *notify.Outbox, chID int64, kind, target, secret string) {
	t.Helper()
	err := ob.Enqueue(context.Background(), chID, map[string]any{
		"channel_kind": kind,
		"target":       target,
		"secret":       secret,
		"body":         "hi",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

type jobState struct {
	status    string
	attempts  int
	lastError string
	nextRetry time.Time
}

// readJobState reads the current state of the single outbox row for chID.
func readJobState(t *testing.T, pool *pgxpool.Pool, chID int64) jobState {
	t.Helper()
	var s jobState
	if err := pool.QueryRow(context.Background(),
		"SELECT status, attempts, last_error, next_retry_at FROM notification_outbox WHERE channel_id = $1",
		chID,
	).Scan(&s.status, &s.attempts, &s.lastError, &s.nextRetry); err != nil {
		t.Fatalf("select job state: %v", err)
	}
	return s
}

// waitForJobState polls the single outbox row for chID until pred is
// satisfied or timeout elapses.
func waitForJobState(t *testing.T, pool *pgxpool.Pool, chID int64, timeout time.Duration, pred func(jobState) bool) jobState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		s := readJobState(t, pool, chID)
		if pred(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for job state condition, last seen: %+v", s)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// forceRetryNow rewrites next_retry_at into the past so a running worker's
// next tick reclaims the job immediately, sidestepping the real minutes/
// hours-long backoff schedule.
func forceRetryNow(t *testing.T, pool *pgxpool.Pool, chID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE notification_outbox SET next_retry_at = now() - interval '1 second' WHERE channel_id = $1 AND status = 'pending'",
		chID); err != nil {
		t.Fatalf("force retry now: %v", err)
	}
}

// advanceRetry forces the job back to "ready to send" and waits for the
// worker to actually pick it up. A naive single forceRetryNow call races
// with the worker: Claim() bumps attempts (and thus last_error is already
// non-empty from the *previous* failure) before MarkRetry writes the real
// backoff, so a single forced update can land between Claim and MarkRetry
// and be immediately clobbered by the worker's own backoff write. Looping
// here keeps forcing next_retry_at into the past until attempts actually
// advances past fromAttempts (or the job gives up and fails).
func advanceRetry(t *testing.T, pool *pgxpool.Pool, chID int64, fromAttempts int) jobState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		forceRetryNow(t, pool, chID)
		time.Sleep(50 * time.Millisecond)
		s := readJobState(t, pool, chID)
		if s.attempts > fromAttempts || s.status == "failed" {
			return s
		}
	}
	t.Fatalf("job did not advance past attempt %d", fromAttempts)
	return jobState{}
}

func runWorker(t *testing.T, w *notify.Worker, ctx context.Context) (done <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(ch)
	}()
	return ch
}

// blockingSender never returns on its own: it blocks until the ctx passed
// to Send is done, then reports that as an error. It stands in for one
// hanging target (dead peer, no timeout) to prove the worker doesn't stall
// forever on it.
type blockingSender struct {
	calls int32
}

func (b *blockingSender) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	atomic.AddInt32(&b.calls, 1)
	<-ctx.Done()
	return ctx.Err()
}

// TestWorkerSendTimeoutDoesNotHang proves that a target which never
// responds does not stall the worker forever: with a short per-send
// timeout configured, the blocking sender's ctx is cancelled, Send returns
// an error, and the job is rescheduled (not left stuck "in flight").
func TestWorkerSendTimeoutDoesNotHang(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	chID := newChannel(t, pool)

	blocker := &blockingSender{}
	w := &notify.Worker{
		Outbox:      ob,
		Senders:     map[string]notify.Sender{"blocker": blocker},
		Interval:    20 * time.Millisecond,
		SendTimeout: 50 * time.Millisecond,
	}
	enqueueJob(t, ob, chID, "blocker", "dest", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := runWorker(t, w, ctx)

	s := waitForJobState(t, pool, chID, 2*time.Second, func(s jobState) bool {
		return s.attempts == 1 && s.lastError != ""
	})
	cancel()
	<-done

	if s.status != "pending" {
		t.Errorf("status = %q, want pending (rescheduled after timeout)", s.status)
	}
	if got := atomic.LoadInt32(&blocker.calls); got == 0 {
		t.Errorf("blocker.calls = %d, want >= 1", got)
	}
}

// flakyMarkSentOutbox — in-memory заглушка Outbox, отдающая ровно одну job
// и умеющая уронить первый вызов MarkSent. Нужна, чтобы проверить сужение
// окна at-least-once (ARCH-M2): после успешного Send воркер должен повторить
// MarkSent при транзиентном сбое БД, а не оставлять job на повторную
// доставку. Реальный notify.Outbox для этого не годится — его MarkSent
// невозможно сделать флаки без модификации схемы.
type flakyMarkSentOutbox struct {
	mu            sync.Mutex
	claimed       bool
	markSentCalls int
	sent          bool
	retryCalls    int
	failCalls     int
}

func (f *flakyMarkSentOutbox) Claim(ctx context.Context, limit int) ([]notify.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimed {
		return nil, nil
	}
	f.claimed = true
	return []notify.Job{{
		ID:        1,
		ChannelID: 1,
		Payload:   map[string]any{"channel_kind": "ok", "target": "dest"},
		Attempts:  1,
	}}, nil
}

func (f *flakyMarkSentOutbox) MarkSent(ctx context.Context, jobID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markSentCalls++
	if f.markSentCalls == 1 {
		return errors.New("transient mark sent failure")
	}
	f.sent = true
	return nil
}

func (f *flakyMarkSentOutbox) MarkRetry(ctx context.Context, jobID int64, sendErr error, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryCalls++
	return nil
}

func (f *flakyMarkSentOutbox) MarkFailed(ctx context.Context, jobID int64, sendErr error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCalls++
	return nil
}

func (f *flakyMarkSentOutbox) snapshot() (markSentCalls, retryCalls, failCalls int, sent bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markSentCalls, f.retryCalls, f.failCalls, f.sent
}

// TestWorkerMarkSentRetriesTransientFailure: Send успешен, MarkSent падает
// первый раз и проходит со второго. Воркер должен повторить MarkSent (>1
// вызова) и в итоге пометить job отправленной, НЕ отправляя её на повторную
// доставку (MarkRetry/MarkFailed не вызываются) — окно двойной доставки
// сужено (ARCH-M2). Тест на заглушке, без testcontainers.
func TestWorkerMarkSentRetriesTransientFailure(t *testing.T) {
	ob := &flakyMarkSentOutbox{}
	ok := &fakeSender{}
	w := &notify.Worker{
		Outbox:   ob,
		Senders:  map[string]notify.Sender{"ok": ok},
		Interval: 20 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runWorker(t, w, ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, _, _, sent := ob.snapshot()
		if sent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timeout waiting for job to be marked sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	markSentCalls, retryCalls, failCalls, sent := ob.snapshot()
	if !sent {
		t.Fatal("job not marked sent")
	}
	if markSentCalls < 2 {
		t.Errorf("markSentCalls = %d, want >= 2 (retry after transient failure)", markSentCalls)
	}
	if retryCalls != 0 || failCalls != 0 {
		t.Errorf("redelivery scheduled: retryCalls=%d failCalls=%d, want 0/0", retryCalls, failCalls)
	}
	if got := atomic.LoadInt32(&ok.calls); got != 1 {
		t.Errorf("Send calls = %d, want 1 (no redelivery of a delivered message)", got)
	}
}

func TestWorkerDeliversSuccessfulJob(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	chID := newChannel(t, pool)

	ok := &fakeSender{}
	w := &notify.Worker{
		Outbox:   ob,
		Senders:  map[string]notify.Sender{"ok": ok},
		Interval: 20 * time.Millisecond,
	}
	enqueueJob(t, ob, chID, "ok", "dest", "sek")

	ctx, cancel := context.WithCancel(context.Background())
	done := runWorker(t, w, ctx)

	waitForJobState(t, pool, chID, 2*time.Second, func(s jobState) bool { return s.status == "sent" })
	cancel()
	<-done

	if got := atomic.LoadInt32(&ok.calls); got != 1 {
		t.Errorf("ok.calls = %d, want 1", got)
	}
	if len(ok.targets) != 1 || ok.targets[0].Target != "dest" || ok.targets[0].Secret != "sek" {
		t.Errorf("target = %+v, want {Target:dest Secret:sek}", ok.targets)
	}
}

func TestWorkerRetriesFailingJob(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	chID := newChannel(t, pool)

	bad := &fakeSender{failAlways: true}
	w := &notify.Worker{
		Outbox:   ob,
		Senders:  map[string]notify.Sender{"bad": bad},
		Interval: 20 * time.Millisecond,
	}
	enqueueJob(t, ob, chID, "bad", "dest", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := runWorker(t, w, ctx)

	s := waitForJobState(t, pool, chID, 2*time.Second, func(s jobState) bool {
		return s.attempts == 1 && s.lastError != ""
	})
	cancel()
	<-done

	if s.status != "pending" {
		t.Errorf("status = %q, want pending", s.status)
	}
	if !s.nextRetry.After(time.Now()) {
		t.Errorf("next_retry_at = %v, want in the future", s.nextRetry)
	}
}

// TestWorkerFailsAfterFiveAttempts drives the worker through its own
// claim/send/backoff loop five times (forcing next_retry_at back into the
// past between attempts, since the real backoff schedule spans minutes to
// hours) and asserts it gives up via MarkFailed on the fifth attempt.
func TestWorkerFailsAfterFiveAttempts(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	chID := newChannel(t, pool)

	bad := &fakeSender{failAlways: true}
	w := &notify.Worker{
		Outbox:   ob,
		Senders:  map[string]notify.Sender{"bad": bad},
		Interval: 20 * time.Millisecond,
	}
	enqueueJob(t, ob, chID, "bad", "dest", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := runWorker(t, w, ctx)
	defer func() { cancel(); <-done }()

	for attempt := 1; attempt <= 4; attempt++ {
		want := attempt
		waitForJobState(t, pool, chID, 2*time.Second, func(s jobState) bool {
			return s.status == "pending" && s.attempts == want
		})
		advanceRetry(t, pool, chID, want)
	}

	s := waitForJobState(t, pool, chID, 2*time.Second, func(s jobState) bool {
		return s.status == "failed"
	})
	if s.attempts != 5 {
		t.Errorf("attempts = %d, want 5", s.attempts)
	}
	if s.lastError == "" {
		t.Errorf("last_error empty, want fake send failure recorded")
	}
	if got := atomic.LoadInt32(&bad.calls); got != 5 {
		t.Errorf("bad.calls = %d, want 5", got)
	}
}
