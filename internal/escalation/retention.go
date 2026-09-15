package escalation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CASE в incident_keep считается один раз на пару (incident_source, incident_id),
// а не на каждую строку журнала — иначе цена умножается на число строк у инцидента.
const purgeEscalationsSQL = `
WITH candidates AS (
	SELECT id, incident_source, incident_id
	FROM incident_escalations
	WHERE sent_at < $1
),
incident_keep AS (
	SELECT s.incident_source, s.incident_id,
		CASE s.incident_source
			WHEN 'host'    THEN EXISTS (SELECT 1 FROM host_incidents      t WHERE t.id = s.incident_id AND (t.resolved_at IS NULL OR t.resolved_at >= $1))
			WHEN 'metric'  THEN EXISTS (SELECT 1 FROM metric_incidents    t WHERE t.id = s.incident_id AND (t.resolved_at IS NULL OR t.resolved_at >= $1))
			WHEN 'trace'   THEN EXISTS (SELECT 1 FROM perf_regressions    t WHERE t.id = s.incident_id AND (t.resolved_at IS NULL OR t.resolved_at >= $1))
			WHEN 'profile' THEN EXISTS (SELECT 1 FROM profile_regressions t WHERE t.id = s.incident_id AND (t.resolved_at IS NULL OR t.resolved_at >= $1))
			WHEN 'slo'     THEN EXISTS (SELECT 1 FROM slo_incidents      t WHERE t.id = s.incident_id AND (t.resolved_at IS NULL OR t.resolved_at >= $1))
			WHEN 'uptime'  THEN EXISTS (SELECT 1 FROM incidents          t WHERE t.id = s.incident_id AND (t.resolved_at IS NULL OR t.resolved_at >= $1))
			ELSE false -- источник не входит в список известных: чистится по возрасту
		END AS keep
	FROM (SELECT DISTINCT incident_source, incident_id FROM candidates) s
),
deleted AS (
	DELETE FROM incident_escalations e
	USING candidates c
	JOIN incident_keep k ON k.incident_source = c.incident_source AND k.incident_id = c.incident_id
	WHERE e.id = c.id AND NOT k.keep
	RETURNING c.incident_source
)
SELECT
	(SELECT count(*) FROM candidates),
	count(*),
	COALESCE(array_agg(DISTINCT incident_source) FILTER (
		WHERE incident_source NOT IN ('host', 'metric', 'trace', 'profile', 'slo', 'uptime')), '{}')
FROM deleted
`

// PurgeResult — итог одного прохода PurgeOldEscalations.
type PurgeResult struct {
	Deleted        int64    // строк удалено суммарно (incident_escalations + escalation_step_log_failures)
	KeptOpen       int64    // старых строк incident_escalations сохранено, потому что их инцидент ещё открыт
	UnknownSources []string // источники incident_escalations, не входящие в список известных, чьи строки удалены по возрасту
}

// escalation_step_log_failures не имеет FK на incident_id и не исчезает вместе
// с инцидентом/проектом — чистится здесь же по возрасту, а не по закрытости.
func PurgeOldEscalations(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (PurgeResult, error) {
	// olderThan <= 0 сдвинул бы cutoff в будущее и удалил бы практически ВСЕ
	// строки обеих таблиц разом — гвард нужен даже если сегодня вызывающий этого не делает.
	if olderThan <= 0 {
		return PurgeResult{}, fmt.Errorf("escalation: purge old: olderThan must be positive, got %s", olderThan)
	}
	cutoff := time.Now().Add(-olderThan)

	var candidates, deleted int64
	var unknown []string
	if err := pool.QueryRow(ctx, purgeEscalationsSQL, cutoff).Scan(&candidates, &deleted, &unknown); err != nil {
		return PurgeResult{}, fmt.Errorf("escalation: purge old: %w", err)
	}

	failTag, err := pool.Exec(ctx, "DELETE FROM escalation_step_log_failures WHERE last_attempt_at < $1", cutoff)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("escalation: purge old log failures: %w", err)
	}

	return PurgeResult{
		Deleted:        deleted + failTag.RowsAffected(),
		KeptOpen:       candidates - deleted,
		UnknownSources: unknown,
	}, nil
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
	res, err := PurgeOldEscalations(ctx, j.Pool, j.Retention)
	if err != nil {
		slog.Error("escalation janitor: purge failed", "error", err)
		return
	}
	if res.Deleted > 0 || res.KeptOpen > 0 {
		slog.Info("escalation janitor: purged old rows", "deleted", res.Deleted, "kept_open", res.KeptOpen)
	}
	if len(res.UnknownSources) > 0 {
		slog.Warn("escalation janitor: purged rows with unknown incident source by age", "sources", res.UnknownSources)
	}
}
