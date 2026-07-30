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

func (a *alwaysFailMarkSent) MarkRetry(ctx context.Context, jobID int64, sendErr error, retryIn time.Duration) error {
	return nil
}

func (a *alwaysFailMarkSent) MarkFailed(ctx context.Context, jobID int64, sendErr error) error {
	return nil
}

// TestMarkSentFinishesDespiteCancel: сообщение уже отправлено получателю, и
// подтвердить это в очереди нужно независимо от того, что процесс выключают.
//
// Прежнее поведение — прервать попытки на отменённом контексте — экономило доли
// секунды на остановке ценой того, что задача оставалась pending и уходила
// получателю повторно после рестарта. Идемпотентности у Telegram и вебхуков
// нет, поэтому дубль виден человеку; задержка выключения на 200 мс — нет.
func TestMarkSentFinishesDespiteCancel(t *testing.T) {
	store := &alwaysFailMarkSent{}
	w := &Worker{Outbox: store}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.markSent(ctx, Job{ID: 1, ChannelID: 1})

	if store.markSentCalls != markSentRetries {
		t.Errorf("markSentCalls = %d, want %d: отмена контекста не должна отменять "+
			"запись результата уже состоявшейся отправки", store.markSentCalls, markSentRetries)
	}
}

// TestRetryOrFailWritesDespiteCancel: то же для неудачной отправки. Раньше при
// остановке процесса MarkRetry падал по отменённому контексту, и задача
// оставалась со сдвинутым next_retry_at — выключение задерживало доставку на
// длину claim-лизы.
func TestRetryOrFailWritesDespiteCancel(t *testing.T) {
	store := &recordingStore{}
	w := &Worker{Outbox: store}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w.retryOrFail(ctx, Job{ID: 1, ChannelID: 1, Attempts: 1}, errors.New("smtp timeout"))

	if store.retryCalls != 1 {
		t.Errorf("MarkRetry вызван %d раз, want 1", store.retryCalls)
	}
	if store.lastRetryErr != nil {
		t.Errorf("MarkRetry получил отменённый контекст: %v", store.lastRetryErr)
	}
}

// recordingStore записывает, что дошло до хранилища, и проверяет, что контекст
// живой.
type recordingStore struct {
	retryCalls   int
	lastRetryErr error
}

func (r *recordingStore) Claim(ctx context.Context, limit int) ([]Job, error) { return nil, nil }
func (r *recordingStore) MarkSent(ctx context.Context, jobID int64) error     { return nil }
func (r *recordingStore) MarkRetry(ctx context.Context, jobID int64, sendErr error, retryIn time.Duration) error {
	r.retryCalls++
	r.lastRetryErr = ctx.Err()
	return nil
}
func (r *recordingStore) MarkFailed(ctx context.Context, jobID int64, sendErr error) error {
	return nil
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
