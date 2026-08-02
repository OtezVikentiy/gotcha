-- backward-compatible: yes (новый индекс)
--
-- Находка №5: entityRules чистит metric_incidents по resolved_at с условием
-- закрытости "status = 'resolved' AND resolved_at IS NOT NULL"
-- (internal/telemetry/entity_janitor.go) — тот же принцип, что у
-- perf_regressions/profile_regressions (0034/0035): открытый инцидент
-- метрики удалению не подлежит. Условие частичного индекса сверено с
-- entityRules дословно, не с планом задачи.
--
-- Один индекс на файл — вывод эксперимента, см. докблок
-- 0031_issues_last_seen_idx.up.sql и task-1-report.md.
CREATE INDEX CONCURRENTLY IF NOT EXISTS metric_incidents_resolved_at_idx
    ON metric_incidents (resolved_at)
    WHERE status = 'resolved' AND resolved_at IS NOT NULL;
