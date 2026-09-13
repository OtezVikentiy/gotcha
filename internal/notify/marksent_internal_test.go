package notify

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Нужна, чтобы прогнать markSent по веткам, недоступным флаки-заглушке из
// worker_test.go (та отдаёт успех со второй попытки).
type alwaysFailMarkSent struct {
	markSentCalls int
}

func (a *alwaysFailMarkSent) Claim(ctx context.Context, limit int) ([]Job, error) {
	return nil, nil
}

func (a *alwaysFailMarkSent) MarkSent(ctx context.Context, jobID int64, attempt int) error {
	a.markSentCalls++
	return errors.New("persistent mark sent failure")
}

func (a *alwaysFailMarkSent) MarkRetry(ctx context.Context, jobID int64, attempt int, sendErr error, retryIn time.Duration) error {
	return nil
}

func (a *alwaysFailMarkSent) MarkFailed(ctx context.Context, jobID int64, attempt int, sendErr error) error {
	return nil
}

// Идемпотентности у Telegram и вебхуков нет — дубль от повторной отправки
// после рестарта виден человеку, задержка выключения на 200мс — нет.
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

type recordingStore struct {
	retryCalls   int
	lastRetryErr error
}

func (r *recordingStore) Claim(ctx context.Context, limit int) ([]Job, error) { return nil, nil }
func (r *recordingStore) MarkSent(ctx context.Context, jobID int64, attempt int) error {
	return nil
}
func (r *recordingStore) MarkRetry(ctx context.Context, jobID int64, attempt int, sendErr error, retryIn time.Duration) error {
	r.retryCalls++
	r.lastRetryErr = ctx.Err()
	return nil
}
func (r *recordingStore) MarkFailed(ctx context.Context, jobID int64, attempt int, sendErr error) error {
	return nil
}

func TestMarkSentExhaustsRetries(t *testing.T) {
	store := &alwaysFailMarkSent{}
	w := &Worker{Outbox: store}

	w.markSent(context.Background(), Job{ID: 1, ChannelID: 1})

	if store.markSentCalls != markSentRetries {
		t.Errorf("markSentCalls = %d, want %d (all retries exhausted)", store.markSentCalls, markSentRetries)
	}
}

// Час vs миллисекунда — три порядка запаса, без риска ложных падений на
// загруженном раннере; ассерт проверяет только «вернулась/зависла».
func TestMarkSentWaitStopsWithContext(t *testing.T) {
	t.Run("отменённый ctx прерывает огромную паузу", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		done := make(chan struct{})
		go func() {
			markSentWait(ctx, time.Hour)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("markSentWait не вернулась при отменённом ctx — с часовой паузой " +
				"досидела бы час вместо того, чтобы прерваться по ctx.Done()")
		}
	})

	t.Run("живой ctx досиживает микроскопическую паузу", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			markSentWait(context.Background(), time.Millisecond)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("markSentWait зависла на живом ctx с миллисекундной паузой — " +
				"ветка time.After должна была сработать")
		}
	})
}
