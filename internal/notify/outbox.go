package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("notify: not found")

// Payload несёт данные для шаблона доставки; Kind/Subject/Body вычисляет
// отправитель канала перед постановкой в очередь.
type Message struct {
	ChannelID int64
	Kind      string
	Subject   string
	Body      string
	Payload   map[string]any
}

type Job struct {
	ID        int64
	ChannelID int64
	Payload   map[string]any
	Attempts  int
}

type Outbox struct {
	pool *pgxpool.Pool
}

func NewOutbox(pool *pgxpool.Pool) *Outbox {
	return &Outbox{pool: pool}
}

func (o *Outbox) Enqueue(ctx context.Context, channelID int64, payload map[string]any) error {
	if _, err := o.pool.Exec(ctx,
		"INSERT INTO notification_outbox (channel_id, payload) VALUES ($1, $2)",
		channelID, payload); err != nil {
		return fmt.Errorf("notify: enqueue: %w", err)
	}
	return nil
}

// Лиза обязана покрывать обработку всего батча с запасом — иначе дубли уйдут
// на два канала; править вместе с constants в worker.go (claimBatchPerWorker).
const claimLease = 5 * time.Minute

// Лиза считается часами БАЗЫ (now() в SQL), а не процесса — иначе отставший
// контейнер выдал бы просроченную лизу, и задача ушла бы дублем на каждом тике.
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

func (o *Outbox) MarkRetry(ctx context.Context, jobID int64, sendErr error, retryIn time.Duration) error {
	// next_retry_at считается часами БАЗЫ (now() в SQL), не процесса — та же
	// причина, что у claim-лизы (см. Claim).
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

// Не несёт payload — может содержать секреты канала (webhook HMAC, telegram
// token, см. TransportFields) — только то, что нужно понять, что не доставилось.
type FailedJob struct {
	ID          int64
	ChannelKind string
	Target      string
	LastError   string
	Attempts    int
	CreatedAt   time.Time
}

// Джойнит alert_channels для человекочитаемых kind/target. payload не селектится
// намеренно (см. FailedJob) — секреты канала не должны уходить в UI.
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

// Один запрос, не три — иначе несогласованный снимок гонки читается как баг.
// Возраст — от created_at (с этого момента ждёт), не от next_retry_at.
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

// pending не трогает. Хранит секреты каналов в payload — без чистки копится бесконечно.
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
