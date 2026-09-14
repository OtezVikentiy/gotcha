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

func enqueueJob(t *testing.T, ob *notify.Outbox, chID int64, kind, target string) {
	t.Helper()
	err := ob.Enqueue(context.Background(), chID, map[string]any{
		"channel_kind": kind,
		"target":       target,
		"body":         "hi",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

type fakeSecrets struct {
	secret string
	err    error
	calls  int32
}

func (f *fakeSecrets) ChannelSecret(context.Context, int64) (string, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.secret, f.err
}

type jobState struct {
	status    string
	attempts  int
	lastError string
	nextRetry time.Time
}

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

func forceRetryNow(t *testing.T, pool *pgxpool.Pool, chID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"UPDATE notification_outbox SET next_retry_at = now() - interval '1 second' WHERE channel_id = $1 AND status = 'pending'",
		chID); err != nil {
		t.Fatalf("force retry now: %v", err)
	}
}

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

type blockingSender struct {
	calls int32
}

func (b *blockingSender) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	atomic.AddInt32(&b.calls, 1)
	<-ctx.Done()
	return ctx.Err()
}

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
	enqueueJob(t, ob, chID, "blocker", "dest")

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

func (f *flakyMarkSentOutbox) MarkSent(ctx context.Context, jobID int64, attempt int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markSentCalls++
	if f.markSentCalls == 1 {
		return errors.New("transient mark sent failure")
	}
	f.sent = true
	return nil
}

func (f *flakyMarkSentOutbox) MarkRetry(ctx context.Context, jobID int64, attempt int, sendErr error, retryIn time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retryCalls++
	return nil
}

func (f *flakyMarkSentOutbox) MarkFailed(ctx context.Context, jobID int64, attempt int, sendErr error) error {
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
	secrets := &fakeSecrets{secret: "sek"}
	w := &notify.Worker{
		Outbox:   ob,
		Senders:  map[string]notify.Sender{"ok": ok},
		Secrets:  secrets,
		Interval: 20 * time.Millisecond,
	}
	enqueueJob(t, ob, chID, "ok", "dest")

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
	if got := atomic.LoadInt32(&secrets.calls); got != 1 {
		t.Errorf("ChannelSecret вызван %d раз, want 1 — секрет обязан резолвиться по channel_id", got)
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
	enqueueJob(t, ob, chID, "bad", "dest")

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
	enqueueJob(t, ob, chID, "bad", "dest")

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

type panicSender struct {
	calls int32
}

func (p *panicSender) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	atomic.AddInt32(&p.calls, 1)
	panic("boom: sender exploded")
}

func TestWorkerRecoversFromPanicInProcess(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ob := notify.NewOutbox(pool)
	badCh := newChannel(t, pool)
	okCh := newChannel(t, pool)

	bad := &panicSender{}
	ok := &fakeSender{}
	w := &notify.Worker{
		Outbox:   ob,
		Senders:  map[string]notify.Sender{"bad": bad, "ok": ok},
		Interval: 20 * time.Millisecond,
	}
	enqueueJob(t, ob, badCh, "bad", "dest")
	enqueueJob(t, ob, okCh, "ok", "dest")

	ctx, cancel := context.WithCancel(context.Background())
	done := runWorker(t, w, ctx)

	s := waitForJobState(t, pool, badCh, 2*time.Second, func(s jobState) bool {
		return s.attempts == 1 && s.lastError != ""
	})
	if s.status != "pending" {
		t.Errorf("panicking job status = %q, want pending (retryable, not sent)", s.status)
	}

	waitForJobState(t, pool, okCh, 2*time.Second, func(s jobState) bool { return s.status == "sent" })

	cancel()
	<-done

	if got := atomic.LoadInt32(&bad.calls); got != 1 {
		t.Errorf("bad.calls = %d, want 1", got)
	}
}

type slowSender struct {
	delay   time.Duration
	inFlt   atomic.Int32
	maxInFl atomic.Int32
	calls   atomic.Int32
}

func (s *slowSender) Send(ctx context.Context, t notify.Target, payload map[string]any) error {
	s.calls.Add(1)
	cur := s.inFlt.Add(1)
	for {
		peak := s.maxInFl.Load()
		if cur <= peak || s.maxInFl.CompareAndSwap(peak, cur) {
			break
		}
	}
	defer s.inFlt.Add(-1)
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
	}
	return nil
}

func TestDeliveryIsConcurrent(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ob := notify.NewOutbox(pool)
	channelID := newChannel(t, pool)

	const jobs = 8
	for i := 0; i < jobs; i++ {
		if err := ob.Enqueue(ctx, channelID, map[string]any{
			"channel_kind": "email", "target": "ops@example.com",
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	sender := &slowSender{delay: 200 * time.Millisecond}
	w := &notify.Worker{
		Outbox:      ob,
		Senders:     map[string]notify.Sender{"email": sender},
		Concurrency: 4,
	}

	w.Tick(ctx)

	if got := sender.calls.Load(); got != jobs {
		t.Fatalf("отправок = %d, want %d", got, jobs)
	}
	if peak := sender.maxInFl.Load(); peak < 2 {
		t.Errorf("пик одновременных отправок = %d: доставка последовательная, "+
			"один медленный канал задержит все остальные", peak)
	}
	if peak := sender.maxInFl.Load(); peak > 4 {
		t.Errorf("пик одновременных отправок = %d, want <= 4: параллелизм не ограничен", peak)
	}
}

func TestTickDrainsQueueWithoutWaitingForNextTick(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ob := notify.NewOutbox(pool)
	channelID := newChannel(t, pool)

	const jobs = 9
	for i := 0; i < jobs; i++ {
		if err := ob.Enqueue(ctx, channelID, map[string]any{
			"channel_kind": "email", "target": "ops@example.com",
		}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	sender := &fakeSender{}
	w := &notify.Worker{
		Outbox:      ob,
		Senders:     map[string]notify.Sender{"email": sender},
		Concurrency: 2,
	}
	w.Tick(ctx)

	if got := atomic.LoadInt32(&sender.calls); got != jobs {
		t.Errorf("отправлено %d из %d за один тик — остальное ждёт следующего", got, jobs)
	}
}
