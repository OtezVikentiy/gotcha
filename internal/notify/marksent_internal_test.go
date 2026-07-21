package notify

import (
	"context"
	"errors"
	"testing"
	"time"
)

// alwaysFailMarkSent — заглушка outboxStore, у которой MarkSent всегда падает.
// Нужна, чтобы прогнать markSent по веткам, недоступным флаки-заглушке из
// worker_test.go (та отдаёт успех со второй попытки).
type alwaysFailMarkSent struct {
	markSentCalls int
}

func (a *alwaysFailMarkSent) Claim(ctx context.Context, limit int) ([]Job, error) {
	return nil, nil
}

func (a *alwaysFailMarkSent) MarkSent(ctx context.Context, jobID int64) error {
	a.markSentCalls++
	return errors.New("persistent mark sent failure")
}

func (a *alwaysFailMarkSent) MarkRetry(ctx context.Context, jobID int64, sendErr error, next time.Time) error {
	return nil
}

func (a *alwaysFailMarkSent) MarkFailed(ctx context.Context, jobID int64, sendErr error) error {
	return nil
}

// TestMarkSentAbortsOnCtxDone: MarkSent падает на первой попытке, а ctx уже
// отменён — backoff-select должен уйти в ветку <-ctx.Done() и прервать ретраи
// немедленно (одна попытка), не подвешивая остановку воркера.
func TestMarkSentAbortsOnCtxDone(t *testing.T) {
	store := &alwaysFailMarkSent{}
	w := &Worker{Outbox: store}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.markSent(ctx, Job{ID: 1, ChannelID: 1})

	if store.markSentCalls != 1 {
		t.Errorf("markSentCalls = %d, want 1 (aborted on ctx.Done after first failure)", store.markSentCalls)
	}
}

// TestMarkSentExhaustsRetries: с живым ctx и всегда падающим MarkSent воркер
// обязан исчерпать все markSentRetries попытки и сдаться (оставив job pending),
// покрывая финальную ветку "mark sent failed after retries".
func TestMarkSentExhaustsRetries(t *testing.T) {
	store := &alwaysFailMarkSent{}
	w := &Worker{Outbox: store}

	w.markSent(context.Background(), Job{ID: 1, ChannelID: 1})

	if store.markSentCalls != markSentRetries {
		t.Errorf("markSentCalls = %d, want %d (all retries exhausted)", store.markSentCalls, markSentRetries)
	}
}
