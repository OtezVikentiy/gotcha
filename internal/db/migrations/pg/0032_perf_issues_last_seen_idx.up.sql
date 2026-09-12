-- backward-compatible: yes (новый индекс)
--
-- Тот же дефект, что у 0031: perf_issues_project_last_seen_idx начинается с project_id
-- и не помогает скану по возрасту. Приём CONCURRENTLY — см. 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS perf_issues_last_seen_idx
    ON perf_issues (last_seen);
