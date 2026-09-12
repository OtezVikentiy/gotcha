package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RetentionChange struct {
	Key      string
	Previous int
	Current  int
}

func (c RetentionChange) Changed() bool { return c.Previous > 0 && c.Previous != c.Current }

// TTL в ClickHouse — свойство инсталляции, а не реплики: расхождение между
// репликами гоняло его туда-обратно — ALTER TABLE MODIFY TTL по всей таблице.
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
