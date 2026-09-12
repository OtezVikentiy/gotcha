-- backward-compatible: yes (новый индекс)
--
-- Открытая регрессия описывает активную проблему — чистильщику удалять её по возрасту
-- нельзя, индекс частичный под то же условие. Приём CONCURRENTLY — см. 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS perf_regressions_resolved_at_idx
    ON perf_regressions (resolved_at)
    WHERE status = 'resolved' AND resolved_at IS NOT NULL;
