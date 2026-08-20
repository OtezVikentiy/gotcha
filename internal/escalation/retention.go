package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PurgeOldEscalations удаляет строки incident_escalations старше olderThan
// (по sent_at) и возвращает число удалённых. Гигиена (M-6): лог эскалаций
// растёт бесконечно, ретенция привязана к сроку хранения инцидентов
// (cfg.IncidentRetentionDays) — та же сущность, тот же срок жизни. Образец —
// notify.Outbox.PurgeOld (outbox.go:238).
func PurgeOldEscalations(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	tag, err := pool.Exec(ctx, "DELETE FROM incident_escalations WHERE sent_at < $1", cutoff)
	if err != nil {
		return 0, fmt.Errorf("escalation: purge old: %w", err)
	}
	return tag.RowsAffected(), nil
}

// defaultJanitorInterval — период тика Janitor по умолчанию.
const defaultJanitorInterval = time.Hour

// Janitor периодически чистит incident_escalations (см. PurgeOldEscalations).
// Образец — notify.OutboxJanitor (internal/notify/janitor.go).
type Janitor struct {
	Pool      *pgxpool.Pool
	Retention time.Duration // старше — удаляется; <= 0 выключает чистку
	Interval  time.Duration // период тика, дефолт 1 час
}

// Run тикает с Interval, на каждом тике зовёт PurgeOldEscalations(Retention).
// Ошибка логируется и не роняет цикл. Retention <= 0 — janitor не запускать
// (см. вызывающий код в main.go, симметрично entityRetention.Any()).
// Запускать как "go j.Run(ctx)".
func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := PurgeOldEscalations(ctx, j.Pool, j.Retention)
			if err != nil {
				slog.Error("escalation janitor: purge failed", "error", err)
				continue
			}
			if n > 0 {
				slog.Info("escalation janitor: purged old rows", "deleted", n)
			}
		}
	}
}
