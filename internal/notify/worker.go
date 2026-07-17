package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// defaultInterval — период тика воркера, если Worker.Interval не задан.
const defaultInterval = 10 * time.Second

// claimBatch — сколько задач Claim'ит воркер за один тик. Держим в паре с
// Outbox.claimLease (outbox.go): claimLease должна покрывать время обработки
// всего батча (claimBatch * defaultSendTimeout + запас), иначе задачи в
// хвосте батча станут "просроченными" и повторно заклеймлены другой репликой
// worker'а, пока первая ещё их отправляет — правьте обе константы вместе.
const claimBatch = 5

// defaultSendTimeout — таймаут одной попытки доставки, если
// Worker.SendTimeout не задан. Без него зависший таргет (мёртвый пир,
// blackhole) блокирует sequential Worker.tick навсегда — задачи
// обрабатываются одна за другой, так что один плохой канал останавливает
// доставку всем остальным.
const defaultSendTimeout = 30 * time.Second

// outboxStore — подмножество *Outbox, которым пользуется Worker. Вынесено в
// интерфейс, чтобы в тестах подменять хранилище заглушкой (например с флаки
// MarkSent для проверки сужения окна at-least-once, ARCH-M2). Боевой код
// передаёт сюда *Outbox.
type outboxStore interface {
	Claim(ctx context.Context, limit int) ([]Job, error)
	MarkSent(ctx context.Context, jobID int64) error
	MarkRetry(ctx context.Context, jobID int64, sendErr error, next time.Time) error
	MarkFailed(ctx context.Context, jobID int64, sendErr error) error
}

// markSentRetries — сколько раз воркер пытается подтвердить доставку
// (MarkSent) после успешного Send, прежде чем сдаться и оставить job
// pending. См. markSent — сужает окно повторной доставки при транзиентном
// сбое БД.
const markSentRetries = 3

// markSentBackoff — короткая пауза между попытками MarkSent. Держим её
// маленькой: это не сетевой ретрай доставки, а повтор локальной записи в БД,
// и всё это время воркер держит claim-лизу (см. outbox.claimLease) и
// блокирует последовательный tick для остальных задач.
const markSentBackoff = 100 * time.Millisecond

// Worker периодически забирает готовые к отправке задачи из Outbox и шлёт
// их через Senders (ключ — Target.Kind / payload["channel_kind"]).
type Worker struct {
	Outbox   outboxStore
	Senders  map[string]Sender
	Interval time.Duration

	// SendTimeout bounds each individual Send call so one hanging target
	// (dead peer, no timeout on its own client) can't stall the whole
	// worker loop. Defaults to defaultSendTimeout if <= 0.
	SendTimeout time.Duration
}

// Run тикает с Worker.Interval (по умолчанию defaultInterval), на каждом
// тике забирает пачку задач и доставляет их. Возвращается, когда ctx
// отменяется.
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
			w.tick(ctx)
		}
	}
}

func (w *Worker) tick(ctx context.Context) {
	jobs, err := w.Outbox.Claim(ctx, claimBatch)
	if err != nil {
		slog.Error("notify worker: claim failed", "error", err)
		return
	}
	for _, job := range jobs {
		w.process(ctx, job)
	}
}

func (w *Worker) process(ctx context.Context, job Job) {
	kind := stringField(job.Payload, "channel_kind")
	target := Target{
		Kind:   kind,
		Target: stringField(job.Payload, "target"),
		Secret: stringField(job.Payload, "secret"),
	}

	sender, ok := w.Senders[kind]
	if !ok {
		w.retryOrFail(ctx, job, fmt.Errorf("notify: no sender registered for channel kind %q", kind))
		return
	}

	timeout := w.SendTimeout
	if timeout <= 0 {
		timeout = defaultSendTimeout
	}
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	err := sender.Send(sendCtx, target, job.Payload)
	cancel()
	if err != nil {
		w.retryOrFail(ctx, job, err)
		return
	}
	w.markSent(ctx, job)
}

// markSent подтверждает доставку в outbox с коротким циклом ретраев.
//
// Доставка построена по модели at-least-once: между успешным Send и
// MarkSent есть окно, в котором падение процесса или сбой записи в БД
// оставит job в статусе pending — и она будет доставлена повторно по
// истечении claim-лизы. Провайдерского idempotency-ключа у Telegram/webhook
// нет, поэтому подавить такой редкий дубль на стороне канала нельзя — это
// осознанный компромисс, задокументированный в аудите (ARCH-M2).
//
// Ретраи ниже сужают окно: транзиентный сбой БД (кратковременная потеря
// соединения, дедлок) переживается на месте за миллисекунды, не дожидаясь
// полного цикла повторной доставки уже отправленного сообщения. Backoff
// прерывается по ctx, чтобы остановка воркера не подвисала. Если MarkSent не
// удаётся и после всех попыток — оставляем job pending (дубль возможен) и
// логируем Error.
func (w *Worker) markSent(ctx context.Context, job Job) {
	var err error
	for attempt := 1; attempt <= markSentRetries; attempt++ {
		if err = w.Outbox.MarkSent(ctx, job.ID); err == nil {
			return
		}
		if attempt == markSentRetries {
			break
		}
		select {
		case <-ctx.Done():
			slog.Error("notify worker: mark sent aborted",
				"job_id", job.ID, "channel_id", job.ChannelID, "error", err)
			return
		case <-time.After(markSentBackoff):
		}
	}
	slog.Error("notify worker: mark sent failed after retries",
		"job_id", job.ID, "channel_id", job.ChannelID, "attempts", markSentRetries, "error", err)
}

// backoff — задержка перед следующей попыткой по номеру попытки (attempts,
// уже включает текущую). Нулевое значение означает "попытки исчерпаны".
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
	delay := backoff(job.Attempts)
	if delay == 0 {
		if err := w.Outbox.MarkFailed(ctx, job.ID, sendErr); err != nil {
			slog.Error("notify worker: mark failed error", "job_id", job.ID, "channel_id", job.ChannelID, "error", err)
		}
		slog.Error("notify worker: job delivery failed permanently",
			"job_id", job.ID, "channel_id", job.ChannelID, "attempts", job.Attempts, "error", sendErr)
		return
	}

	next := time.Now().Add(delay)
	if err := w.Outbox.MarkRetry(ctx, job.ID, sendErr, next); err != nil {
		slog.Error("notify worker: mark retry error", "job_id", job.ID, "channel_id", job.ChannelID, "error", err)
	}
	slog.Warn("notify worker: job delivery failed, will retry",
		"job_id", job.ID, "channel_id", job.ChannelID, "attempts", job.Attempts,
		"next_retry_at", next, "error", sendErr)
}

func stringField(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}
