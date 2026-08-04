package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// defaultInterval — период тика воркера, если Worker.Interval не задан.
const defaultInterval = 10 * time.Second

// defaultConcurrency — сколько задач воркер отправляет одновременно.
//
// Раньше отправка была последовательной, и один мёртвый вебхук с
// 30-секундным таймаутом съедал тик целиком: инцидент, задевший десяток
// мониторов, растягивал оповещения на минуты. Пропускная способность была
// 5 задач за 10 секунд в лучшем случае и 1 задача за 30 секунд в худшем.
const defaultConcurrency = 4

// claimBatchPerWorker — во сколько раз батч больше числа параллельных
// отправок. Запас нужен, чтобы быстрые каналы не простаивали, пока медленные
// доигрывают свой таймаут.
//
// Держим в паре с Outbox.claimLease (outbox.go): лиза должна покрывать время
// обработки всего батча. При 4 параллельных отправках, батче 8 и таймауте 30 с
// это минута против пятиминутной лизы — правьте константы вместе.
const claimBatchPerWorker = 2

// maxTicklessRounds — сколько батчей подряд воркер берёт, не дожидаясь тика.
//
// Пока очередь полна, ждать тик незачем: тик — это пауза на пустой очереди, а
// не квант работы. Предел нужен, чтобы отказ канала (батч всегда полон,
// доставка всегда падает) не превратил воркер в непрерывный цикл по базе.
const maxTicklessRounds = 10

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
	MarkRetry(ctx context.Context, jobID int64, sendErr error, retryIn time.Duration) error
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

// SecretResolver отдаёт расшифрованный секрет канала по его id. Боевая
// реализация — *alert.Service (единственный владелец мастер-ключа); интерфейс
// нужен и для теста, и чтобы notify не зависел от alert.
type SecretResolver interface {
	ChannelSecret(ctx context.Context, channelID int64) (string, error)
}

// Worker периодически забирает готовые к отправке задачи из Outbox и шлёт
// их через Senders (ключ — Target.Kind / payload["channel_kind"]).
type Worker struct {
	Outbox   outboxStore
	Senders  map[string]Sender
	Interval time.Duration

	// Secrets достаёт секрет канала в момент отправки. В payload задачи
	// секрета больше нет: notification_outbox.payload — обычный jsonb, и
	// хранение там расшифрованного bot-токена сводило на нет шифрование
	// alert_channels.secret (`SELECT payload->>'secret'` отдавал живые токены
	// за всё окно хранения очереди). nil допустим только для каналов, которым
	// секрет не нужен вовсе (email).
	Secrets SecretResolver

	// SendTimeout bounds each individual Send call so one hanging target
	// (dead peer, no timeout on its own client) can't stall the whole
	// worker loop. Defaults to defaultSendTimeout if <= 0.
	SendTimeout time.Duration

	// Concurrency — сколько задач отправляется одновременно. 0 означает
	// defaultConcurrency.
	Concurrency int

	// Stats — счётчики доставки для самотелеметрии. nil допустим: продукт
	// работает и без наблюдения за собой, просто хуже диагностируется.
	Stats *Stats
}

// concurrency — сколько отправок идёт одновременно.
func (w *Worker) concurrency() int {
	if w.Concurrency > 0 {
		return w.Concurrency
	}
	return defaultConcurrency
}

// batchSize — сколько задач забирается за один Claim.
func (w *Worker) batchSize() int { return w.concurrency() * claimBatchPerWorker }

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
			w.Tick(ctx)
		}
	}
}

// Tick — один проход доставки: забирает батчи и отправляет их, пока очередь не
// опустеет (или пока не исчерпан maxTicklessRounds). Экспортирован по той же
// причине, что telemetry.EntityJanitor.Tick: проход должен проверяться целиком,
// а не через ожидание тикера.
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
			// Очередь разобрана — до следующего тика делать нечего.
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// deliver отправляет батч, держа не более concurrency отправок одновременно.
//
// Параллельно, потому что медленный канал не должен задерживать быстрые: они
// друг о друге ничего не знают, и очерёдность между ними ничего не значит.
func (w *Worker) deliver(ctx context.Context, jobs []Job) {
	sem := make(chan struct{}, w.concurrency())
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(job Job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			w.process(ctx, job)
		}(job)
	}
	wg.Wait()
}

func (w *Worker) process(ctx context.Context, job Job) {
	// Сама доставка (выбор отправителя, секрет, таймаут) — общая с
	// синхронной тест-отправкой из настроек канала, см. Direct (№69).
	d := Direct{Senders: w.Senders, Secrets: w.Secrets, SendTimeout: w.SendTimeout}
	kind := stringField(job.Payload, "channel_kind")
	target := stringField(job.Payload, "target")
	if err := d.Send(ctx, job.ChannelID, kind, target, job.Payload); err != nil {
		w.retryOrFail(ctx, job, err)
		return
	}
	w.markSent(ctx, job)
}

// finalizeTimeout — бюджет на запись результата уже выполненной отправки.
const finalizeTimeout = 5 * time.Second

// finalizeCtx — контекст для записи результата отправки.
//
// Не наследует отмену: отмена означает «не начинай новое», а не «брось начатое
// незаписанным». Раньше при остановке процесса MarkRetry/MarkFailed падали по
// тому же отменённому контексту, и до пяти задач оставались со сдвинутым
// next_retry_at — то есть выключение процесса задерживало доставку на длину
// claim-лизы. Тот же приём применён в db.WithMigrationLock для снятия
// advisory-лока.
func finalizeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), finalizeTimeout)
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
// полного цикла повторной доставки уже отправленного сообщения. Сами попытки
// не прерываются отменой ctx — см. TestMarkSentFinishesDespiteCancel: письмо
// уже ушло получателю, и подтвердить это в очереди нужно независимо от того,
// что процесс выключают (идемпотентности у Telegram/webhook нет, дубль виден
// человеку — задержка выключения на 200мс нет). Прерывается по ctx только
// пауза МЕЖДУ попытками: досиживать её вслепую при уже мёртвом ctx незачем,
// раз следующая попытка всё равно случится. Если MarkSent не удаётся и после
// всех попыток — оставляем job pending (дубль возможен) и логируем Error.
func (w *Worker) markSent(ctx context.Context, job Job) {
	var err error
	for attempt := 1; attempt <= markSentRetries; attempt++ {
		markCtx, cancel := finalizeCtx(ctx)
		err = w.Outbox.MarkSent(markCtx, job.ID)
		cancel()
		if err == nil {
			w.count(func(s *Stats) { s.countSent() })
			return
		}
		if attempt == markSentRetries {
			break
		}
		// ctx — ВНЕШНИЙ, не markCtx/finalizeCtx выше: тот снят через
		// context.WithoutCancel специально, чтобы не видеть отмену, и если бы
		// ожидание слушало его, пауза снова перестала бы прерываться — но
		// незаметно, потому что markCtx.Done() почти никогда не сработает
		// раньше пятисекундного дедлайна finalizeTimeout.
		markSentWait(ctx, markSentBackoff)
	}
	slog.Error("notify worker: mark sent failed after retries",
		"job_id", job.ID, "channel_id", job.ChannelID, "attempts", markSentRetries, "error", err)
}

// markSentWait — пауза между попытками MarkSent внутри markSent, вынесенная
// отдельной функцией ради тестируемости (см. TestMarkSentWaitStopsWithContext):
// подменять markSentBackoff ради теста не хотим — это продуктовая константа,
// не тестовый рычаг.
//
// Прерывается только ожидание, а не сама попытка: попытки MarkSent должны
// пройти все до конца даже при отменённом ctx (см. TestMarkSentFinishesDespiteCancel
// — сообщение уже отправлено получателю, и подтвердить это в очереди нужно
// независимо от остановки процесса; у Telegram/webhook нет idempotency-ключа,
// дубль виден человеку, задержка выключения на 200мс — нет). Но досиживать
// паузу вслепую при уже мёртвом ctx незачем, раз следующая попытка всё равно
// случится — отсюда select с ctx.Done() как выходом.
func markSentWait(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
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
	// Результат уже состоявшейся попытки записывается независимо от отмены:
	// иначе остановка процесса оставляет задачу с уже сдвинутым next_retry_at.
	markCtx, cancel := finalizeCtx(ctx)
	defer cancel()

	delay := backoff(job.Attempts)
	if delay == 0 {
		if err := w.Outbox.MarkFailed(markCtx, job.ID, sendErr); err != nil {
			slog.Error("notify worker: mark failed error", "job_id", job.ID, "channel_id", job.ChannelID, "error", err)
		}
		w.count(func(s *Stats) { s.countFailed() })
		slog.Error("notify worker: job delivery failed permanently",
			"job_id", job.ID, "channel_id", job.ChannelID, "attempts", job.Attempts, "error", sendErr)
		return
	}

	if err := w.Outbox.MarkRetry(markCtx, job.ID, sendErr, delay); err != nil {
		slog.Error("notify worker: mark retry error", "job_id", job.ID, "channel_id", job.ChannelID, "error", err)
	}
	w.count(func(s *Stats) { s.countRetried() })
	slog.Warn("notify worker: job delivery failed, will retry",
		"job_id", job.ID, "channel_id", job.ChannelID, "attempts", job.Attempts,
		"retry_in", delay, "error", sendErr)
}

// count применяет изменение к счётчикам, если наблюдение включено.
func (w *Worker) count(fn func(*Stats)) {
	if w.Stats != nil {
		fn(w.Stats)
	}
}

func stringField(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}
