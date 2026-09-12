-- backward-compatible: yes (новый индекс)
--
-- Тот же принцип, что у 0033-0035: открытый инцидент чистильщику удалению не подлежит.
-- Приём CONCURRENTLY — см. 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS metric_incidents_resolved_at_idx
    ON metric_incidents (resolved_at)
    WHERE status = 'resolved' AND resolved_at IS NOT NULL;
