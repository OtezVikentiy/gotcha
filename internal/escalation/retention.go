package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// escalation_step_log_failures не имеет FK на incident_id и не исчезает вместе
// с инцидентом/проектом — чистится здесь же, а не отдельным ретеншеном.
func PurgeOldEscalations(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	// olderThan <= 0 сдвинул бы cutoff в будущее и удалил бы практически ВСЕ
	// строки обеих таблиц разом — гвард нужен даже если сегодня вызывающий этого не делает.
	if olderThan <= 0 {
		return 0, fmt.Errorf("escalation: purge old: olderThan must be positive, got %s", olderThan)
	}
	cutoff := time.Now().Add(-olderThan)
	tag, err := pool.Exec(ctx, "DELETE FROM incident_escalations WHERE sent_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("escalation: purge old: %w", err)
	}
	failTag, err := pool.Exec(ctx, "DELETE FROM escalation_step_log_failures WHERE last_attempt_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("escalation: purge old log failures: %w", err)
	}
	return tag.RowsAffected() + failTag.RowsAffected(), nil
}

const defaultJanitorInterval = time.Hour

type Janitor struct {
	Pool      *pgxpool.Pool
	Retention time.Duration // старше — удаляется; <= 0 выключает чистку
	Interval  time.Duration // период тика, дефолт 1 час
}

// Запускать как "go j.Run(ctx)". Ошибка тика логируется и не роняет цикл.
func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первый проход сразу — иначе после каждого рестарта чаще Interval лог не чистится вовсе.
	j.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			j.tick(ctx)
		}
	}
}

func (j *Janitor) tick(ctx context.Context) {
	n, err := PurgeOldEscalations(ctx, j.Pool, j.Retention)
	if err != nil {
		slog.Error("escalation janitor: purge failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("escalation janitor: purged old rows", "deleted", n)
	}
}
