package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RetentionChange — что произошло с настройкой ретенции при старте реплики.
type RetentionChange struct {
	// Key — какая именно ретенция (events, spans, metrics, profiles).
	Key string
	// Previous — значение, применённое к инсталляции раньше; 0 — впервые.
	Previous int
	// Current — значение этой реплики.
	Current int
}

// Changed — отличается ли значение реплики от применённого раньше.
func (c RetentionChange) Changed() bool { return c.Previous > 0 && c.Previous != c.Current }

// RecordRetention фиксирует применённые сроки хранения и сообщает, какие из них
// разошлись с тем, что было применено раньше.
//
// Существует потому, что TTL в ClickHouse — свойство ИНСТАЛЛЯЦИИ, а задавался
// он окружением КАЖДОЙ реплики. Две реплики с разными значениями перекидывали
// TTL туда-обратно на каждом рестарте, и каждый переброс — это ALTER TABLE
// MODIFY TTL, то есть пересчёт по всем кускам таблицы. Молча.
//
// Запись не мешает менять ретенцию: значение применяется как раньше. Она делает
// расхождение видимым — вызывающий пишет о нём в лог, и «инстанс сам себе
// перекидывает TTL» перестаёт быть невидимым событием.
func RecordRetention(ctx context.Context, pool *pgxpool.Pool, values map[string]int) ([]RetentionChange, error) {
	out := make([]RetentionChange, 0, len(values))
	for key, days := range values {
		var prev int
		err := pool.QueryRow(ctx, "SELECT days FROM retention_state WHERE key = $1", key).Scan(&prev)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			prev = 0
		case err != nil:
			return nil, fmt.Errorf("retention state: read %s: %w", key, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO retention_state (key, days) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET days = EXCLUDED.days, applied_at = now()`,
			key, days); err != nil {
			return nil, fmt.Errorf("retention state: record %s: %w", key, err)
		}
		out = append(out, RetentionChange{Key: key, Previous: prev, Current: days})
	}
	return out, nil
}
