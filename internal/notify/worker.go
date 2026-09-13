package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const defaultInterval = 10 * time.Second

const defaultConcurrency = 4

// Держать в паре с Outbox.claimLease: лиза обязана покрывать время обработки батча.
const claimBatchPerWorker = 2

const maxTicklessRounds = 10

const defaultSendTimeout = 30 * time.Second

type outboxStore interface {
	Claim(ctx context.Context, limit int) ([]Job, error)
	MarkSent(ctx context.Context, jobID int64, attempt int) error
	MarkRetry(ctx context.Context, jobID int64, attempt int, sendErr error, retryIn time.Duration) error
	MarkFailed(ctx context.Context, jobID int64, attempt int, sendErr error) error
}

const markSentRetries = 3

const markSentBackoff = 100 * time.Millisecond

type SecretResolver interface {
	ChannelSecret(ctx context.Context, channelID int64) (string, error)
}

type Worker struct {
	Outbox   outboxStore
	Senders  map[string]Sender
	Interval time.Duration

	Secrets SecretResolver

	SendTimeout time.Duration

	Concurrency int

	Stats *Stats
}

func (w *Worker) concurrency() int {
	if w.Concurrency > 0 {
		return w.Concurrency
	}
	return defaultConcurrency
}

func (w *Worker) batchSize() int { return w.concurrency() * claimBatchPerWorker }

func (w *Worker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

func (w *Worker) Tick(ctx context.Context) {
	for round := 0; round < maxTicklessRounds; round++ {
		batch := w.batchSize()
		jobs, err := w.Outbox.Claim(ctx, batch)
		if err != nil {
			slog.Error("notify worker: claim failed", "error", err)
			return
		}
		if len(jobs) == 0 {
			return
		}
		w.deliver(ctx, jobs)
		if len(jobs) < batch {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (w *Worker) deliver(ctx context.Context, jobs []Job) {
	sem := make(chan struct{}, w.concurrency())
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job Job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			w.processGuarded(ctx, job)
		}(job)
	}
	wg.Wait()
}

func (w *Worker) processGuarded(ctx context.Context, job Job) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("notify worker: delivery panicked",
				"job_id", job.ID, "channel_id", job.ChannelID, "panic", r)
			w.retryOrFail(ctx, job, fmt.Errorf("panic: %v", r))
		}
	}()
	w.process(ctx, job)
}

// markSent — только после успешной отправки: до неё job обязана остаться pending,
// у Telegram/webhook нет идемпотентности, повтор после падения процесса даёт видимый дубль.
func (w *Worker) process(ctx context.Context, job Job) {
	d := Direct{Senders: w.Senders, Secrets: w.Secrets, SendTimeout: w.SendTimeout}
	kind := stringField(job.Payload, "channel_kind")
	target := stringField(job.Payload, "target")
	if err := d.Send(ctx, job.ChannelID, kind, target, job.Payload); err != nil {
		w.retryOrFail(ctx, job, err)
		return
	}
	w.markSent(ctx, job)
}

const finalizeTimeout = 5 * time.Second

// WithoutCancel: иначе остановка процесса оставляет задачу с уже сдвинутым next_retry_at.
func finalizeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
}

func (w *Worker) markSent(ctx context.Context, job Job) {
	var err error
	for attempt := 1; attempt <= markSentRetries; attempt++ {
		markCtx, cancel := finalizeCtx(ctx)
		err = w.Outbox.MarkSent(markCtx, job.ID, job.Attempts)
		cancel()
		if err == nil {
			w.count(func(s *Stats) { s.countSent() })
			return
		}
		if attempt == markSentRetries {
			break
		}
		markSentWait(ctx, markSentBackoff)
	}
	slog.Error("notify worker: mark sent failed after retries",
		"job_id", job.ID, "channel_id", job.ChannelID, "attempts", markSentRetries, "error", err)
}

func markSentWait(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func backoff(attempts int) time.Duration {
	switch attempts {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 25 * time.Minute
	case 4:
		return 2 * time.Hour
	default:
		return 0
	}
}

func (w *Worker) retryOrFail(ctx context.Context, job Job, sendErr error) {
	markCtx, cancel := finalizeCtx(ctx)
	defer cancel()

	delay := backoff(job.Attempts)
	if delay == 0 {
		if err := w.Outbox.MarkFailed(markCtx, job.ID, job.Attempts, sendErr); err != nil {
			slog.Error("notify worker: mark failed error", "job_id", job.ID, "channel_id", job.ChannelID, "error", err)
		}
		w.count(func(s *Stats) { s.countFailed() })
		slog.Error("notify worker: job delivery failed permanently",
			"job_id", job.ID, "channel_id", job.ChannelID, "attempts", job.Attempts, "error", sendErr)
		return
	}

	if err := w.Outbox.MarkRetry(markCtx, job.ID, job.Attempts, sendErr, delay); err != nil {
		slog.Error("notify worker: mark retry error", "job_id", job.ID, "channel_id", job.ChannelID, "error", err)
	}
	w.count(func(s *Stats) { s.countRetried() })
	slog.Warn("notify worker: job delivery failed, will retry",
		"job_id", job.ID, "channel_id", job.ChannelID, "attempts", job.Attempts,
		"retry_in", delay, "error", sendErr)
}

func (w *Worker) count(fn func(*Stats)) {
	if w.Stats != nil {
		fn(w.Stats)
	}
}

func stringField(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}
