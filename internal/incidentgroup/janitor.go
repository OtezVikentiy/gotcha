package incidentgroup

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultSweepInterval — период sweep-тика (§4.3: ~1 мин).
const defaultSweepInterval = time.Minute

// retentionEvery — как часто гонять ретеншен-часть (раз в час, §4.3).
const retentionEvery = time.Hour

// SweepOrphanGroups закрывает открытые группы, чей корневой инцидент закрыт
// ИЛИ отсутствует (узел удалён каскадом host_incidents.host_id ON DELETE
// CASCADE, 0066; или гонка «корень закрылся между DownRoot-снимком и
// EnsureGroup»). Без этого члены такой группы молчали бы вечно — нарушение
// fail-noisy (BLOCKER-2). Возвращает число закрытых групп.
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

// PurgeOldGroups удаляет resolved-группы старше olderThan (MAJOR-5), но
// НИКОГДА группу с ещё открытым членом (любого из 4 источников) — R2b/W5.
// Пока строка группы жива, гейты уведомлений (host/metric/slo — Р9 «лесенка
// эскалации») считают точку отсчёта как GREATEST(started_at,
// COALESCE(g.resolved_at, started_at)); если удалить группу под открытым
// членом, LEFT JOIN даёт NULL, COALESCE схлопывается к started_at, и elapsed
// планировщика скачком возвращается к возрасту инцидента — ровно тот
// анти-залповый эффект, против которого сделана лесенка. group_id инцидента
// остаётся висячим только у УЖЕ закрытых членов — это допустимо и раньше:
// все join'ы LEFT, «группа удалена» ≡ «группа закрыта» для всех гейтов
// (см. OpenUnacked).
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

// Janitor — периодическая уборка групп (образец — escalation.Janitor):
// sweep каждый тик (fail-noisy: работает ВСЕГДА, независимо от ретеншена),
// ретеншен — раз в retentionEvery и только при Retention > 0.
type Janitor struct {
	Pool          *pgxpool.Pool
	Retention     time.Duration // resolved-группы старше — удаляются; <= 0 выключает ТОЛЬКО ретеншен
	SweepInterval time.Duration // период sweep, дефолт 1 мин

	lastPurge time.Time
}

// Run тикает до отмены ctx. Запускать как "go j.Run(ctx)".
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
