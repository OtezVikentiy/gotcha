-- backward-compatible: yes (новый индекс)
--
-- Тот же принцип, что у 0034: открытая регрессия удалению не подлежит. Приём CONCURRENTLY — см. 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS profile_regressions_resolved_at_idx
    ON profile_regressions (resolved_at)
    WHERE status = 'resolved' AND resolved_at IS NOT NULL;
