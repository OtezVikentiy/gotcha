package incidentgroup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultSweepInterval = time.Minute

const retentionEvery = time.Hour

// Без этого члены такой группы молчали бы вечно — нарушение fail-noisy.
func SweepOrphanGroups(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE incident_groups g SET resolved_at = now()
		WHERE g.resolved_at IS NULL
		  AND (
			(g.root_source = 'host' AND NOT EXISTS (
				SELECT 1 FROM host_incidents hi
				WHERE hi.id = g.root_incident_id AND hi.status = 'open'))
			OR
			(g.root_source = 'uptime' AND NOT EXISTS (
				SELECT 1 FROM incidents ui
				WHERE ui.id = g.root_incident_id AND ui.resolved_at IS NULL))
		  )`)
	if err != nil {
		return 0, fmt.Errorf("incidentgroup: sweep orphan groups: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Никогда группу с открытым членом — эскалация считает elapsed от GREATEST(started_at, resolved_at).
// Удали её под открытым членом — COALESCE схлопнется к started_at, и лесенка скачком обнулится.
func PurgeOldGroups(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	tag, err := pool.Exec(ctx, `
		DELETE FROM incident_groups g
		WHERE g.resolved_at IS NOT NULL AND g.resolved_at < $1
		  AND NOT EXISTS (SELECT 1 FROM host_incidents   hi WHERE hi.group_id = g.id AND hi.status = 'open')
		  AND NOT EXISTS (SELECT 1 FROM incidents        ui WHERE ui.group_id = g.id AND ui.resolved_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM metric_incidents  mi WHERE mi.group_id = g.id AND mi.status = 'open')
		  AND NOT EXISTS (SELECT 1 FROM slo_incidents     si WHERE si.group_id = g.id AND si.status = 'open')`,
		cutoff)
	if err != nil {
		return 0, fmt.Errorf("incidentgroup: purge old groups: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Sweep — каждый тик, fail-noisy; ретеншен — раз в retentionEvery и только при Retention > 0.
type Janitor struct {
	Pool          *pgxpool.Pool
	Retention     time.Duration // resolved-группы старше — удаляются; <= 0 выключает ТОЛЬКО ретеншен
	SweepInterval time.Duration // период sweep, дефолт 1 мин

	lastPurge time.Time
}

// Тикает до отмены ctx — запускать как "go j.Run(ctx)".
func (j *Janitor) Run(ctx context.Context) {
	interval := j.SweepInterval
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := SweepOrphanGroups(ctx, j.Pool); err != nil {
				slog.Error("incidentgroup janitor: sweep failed", "error", err)
			} else if n > 0 {
				slog.Info("incidentgroup janitor: closed orphan groups", "count", n)
			}
			if j.Retention > 0 && time.Since(j.lastPurge) >= retentionEvery {
				j.lastPurge = time.Now()
				if n, err := PurgeOldGroups(ctx, j.Pool, j.Retention); err != nil {
					slog.Error("incidentgroup janitor: purge failed", "error", err)
				} else if n > 0 {
					slog.Info("incidentgroup janitor: purged old groups", "deleted", n)
				}
			}
		}
	}
}
