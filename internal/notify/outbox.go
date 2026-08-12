// Package notify — очередь исходящих уведомлений (notification_outbox):
// постановка, конкурентный забор задач и учёт попыток доставки. Не знает
// про alert (правила/каналы) — работает с channel_id и произвольным
// payload, чтобы не тянуть зависимость и не создавать цикл импортов.
package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("notify: not found")

// Message — сообщение для отправки через канал. Payload несёт данные для
// шаблона доставки (issue title, url и т.п.); Kind/Subject/Body — то, что
// вычисляет отправитель канала перед постановкой в очередь.
type Message struct {
	ChannelID int64
	Kind      string
	Subject   string
	Body      string
	Payload   map[string]any
}

// Job — задача, забранная из очереди Claim'ом.
type Job struct {
	ID        int64
	ChannelID int64
	Payload   map[string]any
	Attempts  int
}

// Outbox — очередь notification_outbox поверх PostgreSQL.
type Outbox struct {
	pool *pgxpool.Pool
}

func NewOutbox(pool *pgxpool.Pool) *Outbox {
	return &Outbox{pool: pool}
}

// Enqueue ставит уведомление в очередь для доставки через channelID.
func (o *Outbox) Enqueue(ctx context.Context, channelID int64, payload map[string]any) error {
	if _, err := o.pool.Exec(ctx,
		"INSERT INTO notification_outbox (channel_id, payload) VALUES ($1, $2)",
		channelID, payload); err != nil {
		return fmt.Errorf("notify: enqueue: %w", err)
	}
	return nil
}

// claimLease — на сколько Claim отодвигает next_retry_at забранных задач.
// Статус в схеме не различает pending/in-flight (CHECK допускает только
// pending/sent/failed), поэтому "задача уже забрана, но ещё не
// подтверждена" моделируется именно сдвигом next_retry_at вперёд: пока
// воркер не вызвал MarkSent/MarkRetry/MarkFailed, задача не попадает под
// условие выборки и не будет забрана повторно. Если воркер упал, не успев
// подтвердить, задача снова станет видимой по истечении лизы.
//
// Держим в паре с Worker (worker.go): один тик воркера отправляет батч
// параллельно (defaultConcurrency штук одновременно), каждая попытка — до
// defaultSendTimeout (30s). claimLease обязана покрывать время обработки
// всего батча с запасом — иначе задачи в хвосте батча станут reclaimable
// другой репликой, пока первая ещё их отправляет (то есть один и тот же
// outbox-джоб уйдёт дублем на два канала). При defaultConcurrency=4,
// claimBatchPerWorker=2 (батч 8) и defaultSendTimeout=30s это минута
// худшего случая (см. точный расчёт в worker.go:claimBatchPerWorker);
// 5 минут оставляют комфортный запас. Правьте константы вместе.
const claimLease = 5 * time.Minute

// Claim забирает до limit задач, готовых к отправке (status=pending,
// next_retry_at <= now()), помечает их attempts+1, отодвигает
// next_retry_at на claimLease (см. её комментарий) и возвращает. FOR UPDATE
// SKIP LOCKED внутри CTE делает выборку безопасной под конкуренцией: две
// горутины (или процесса), вызывающие Claim одновременно, никогда не
// получат одну и ту же задачу.
// Лиза отсчитывается часами БАЗЫ, теми же, что решают видимость задачи в WHERE
// этого же запроса. Раньше здесь стояло time.Now().Add(claimLease) — часы
// процесса, — и при отставании контейнера лиза выдавалась уже просроченной:
// задача оставалась видимой и доставлялась повторно каждым воркером на каждом
// тике, а идемпотентности ни у Telegram, ни у вебхуков нет. Контейнер без NTP —
// обычное дело, и расхождение в минуты никто не считает аварией.
func (o *Outbox) Claim(ctx context.Context, limit int) ([]Job, error) {
	rows, err := o.pool.Query(ctx, `
		WITH c AS (
			SELECT id FROM notification_outbox
			WHERE status = 'pending' AND next_retry_at <= now()
			ORDER BY next_retry_at
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE notification_outbox o
		SET attempts = attempts + 1, next_retry_at = now() + $2::interval
		FROM c
		WHERE o.id = c.id
		RETURNING o.id, o.channel_id, o.payload, o.attempts`, limit, claimLease.String())
	if err != nil {
		return nil, fmt.Errorf("notify: claim: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.ChannelID, &j.Payload, &j.Attempts); err != nil {
			return nil, fmt.Errorf("notify: claim: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkSent помечает задачу отправленной.
func (o *Outbox) MarkSent(ctx context.Context, jobID int64) error {
	tag, err := o.pool.Exec(ctx,
		"UPDATE notification_outbox SET status = 'sent', sent_at = now() WHERE id = $1", jobID)
	if err != nil {
		return fmt.Errorf("notify: mark sent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRetry возвращает задачу в pending с записанной ошибкой и временем
// следующей попытки.
func (o *Outbox) MarkRetry(ctx context.Context, jobID int64, sendErr error, retryIn time.Duration) error {
	// Отметку, с которой сравнивает база при следующем Claim, ставит база: часы
	// процесса могут спешить или отставать, и тогда повтор случается раньше или
	// позже назначенного — по той же причине, по которой claim-лиза считается
	// в SQL (см. Claim).
	tag, err := o.pool.Exec(ctx, `
		UPDATE notification_outbox
		SET status = 'pending', last_error = $2, next_retry_at = now() + $3::interval
		WHERE id = $1`, jobID, errString(sendErr), retryIn.String())
	if err != nil {
		return fmt.Errorf("notify: mark retry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFailed помечает задачу окончательно неудавшейся (попытки исчерпаны).
func (o *Outbox) MarkFailed(ctx context.Context, jobID int64, sendErr error) error {
	tag, err := o.pool.Exec(ctx, `
		UPDATE notification_outbox SET status = 'failed', last_error = $2 WHERE id = $1`,
		jobID, errString(sendErr))
	if err != nil {
		return fmt.Errorf("notify: mark failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// FailedJob — failed-запись очереди для отображения в UI настроек проекта
// (spec §7: failed-уведомления должны быть видны). Не несёт payload
// (payload может содержать секреты вроде webhook HMAC-ключа или telegram
// bot token — см. TransportFields в webhook.go) — только то, что нужно
// администратору, чтобы понять, что и куда не доставилось: тип канала,
// адресат из alert_channels.target, число попыток и последнюю ошибку.
type FailedJob struct {
	ID          int64
	ChannelKind string
	Target      string
	LastError   string
	Attempts    int
	CreatedAt   time.Time
}

// FailedForProject возвращает до limit последних failed-задач проекта
// projectID (самые новые первыми), джойня alert_channels по channel_id,
// чтобы отдать человекочитаемые kind/target вместо голого channel_id.
// Намеренно НЕ селектит payload (см. FailedJob) — секреты канала туда
// попадают транзитом для воркера (см. worker.go's process) и не должны
// уходить дальше в UI.
func (o *Outbox) FailedForProject(ctx context.Context, projectID int64, limit int) ([]FailedJob, error) {
	rows, err := o.pool.Query(ctx, `
		SELECT o.id, c.kind, c.target, o.last_error, o.attempts, o.created_at
		FROM notification_outbox o
		JOIN alert_channels c ON c.id = o.channel_id
		WHERE c.project_id = $1 AND o.status = 'failed'
		ORDER BY o.id DESC
		LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("notify: failed for project: %w", err)
	}
	defer rows.Close()
	var out []FailedJob
	for rows.Next() {
		var f FailedJob
		if err := rows.Scan(&f.ID, &f.ChannelKind, &f.Target, &f.LastError, &f.Attempts, &f.CreatedAt); err != nil {
			return nil, fmt.Errorf("notify: failed for project: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// QueueSnapshot читает состояние очереди одним запросом.
//
// Один запрос, а не три: снимок должен быть согласованным, иначе «ждёт 0 задач,
// возраст старейшей 4 часа» выглядит как ошибка продукта, а не как гонка между
// тремя запросами.
//
// Возраст считается от created_at, а не от next_retry_at: интересует, сколько
// уведомление ждёт с момента, когда о нём стало известно, а не сколько осталось
// до следующей попытки.
func (o *Outbox) QueueSnapshot(ctx context.Context) (QueueSnapshot, error) {
	var snap QueueSnapshot
	var oldestSecs float64
	err := o.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'failed'),
			coalesce(extract(epoch FROM now() - min(created_at) FILTER (WHERE status = 'pending')), 0)
		FROM notification_outbox`).Scan(&snap.Pending, &snap.Failed, &oldestSecs)
	if err != nil {
		return QueueSnapshot{}, fmt.Errorf("notify: queue snapshot: %w", err)
	}
	if oldestSecs > 0 {
		snap.OldestPendingAge = time.Duration(oldestSecs * float64(time.Second))
	}
	return snap, nil
}

// PurgeOld удаляет доставленные (sent) и окончательно проваленные (failed)
// задачи старше olderThan. pending не трогает. Гигиена: строки outbox несут
// секреты каналов в payload и без чистки копятся бесконечно. Возвращает число
// удалённых.
func (o *Outbox) PurgeOld(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	tag, err := o.pool.Exec(ctx,
		"DELETE FROM notification_outbox WHERE status IN ('sent','failed') AND created_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("notify: purge outbox: %w", err)
	}
	return tag.RowsAffected(), nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
