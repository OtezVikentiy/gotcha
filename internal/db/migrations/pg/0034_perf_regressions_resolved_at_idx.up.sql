-- backward-compatible: yes (новый индекс)
--
-- Находка №5: entityRules чистит perf_regressions по resolved_at с условием
-- закрытости "status = 'resolved' AND resolved_at IS NOT NULL"
-- (internal/telemetry/entity_janitor.go) — открытая регрессия ('open')
-- описывает активную проблему, удалять её по возрасту нельзя. Условие
-- частичного индекса сверено с entityRules дословно (не с планом задачи —
-- план мог устареть), иначе планировщик индекс не возьмёт и вся работа
-- окажется напрасной при зелёных тестах.
--
-- Один индекс на файл — вывод эксперимента, см. докблок
-- 0031_issues_last_seen_idx.up.sql и task-1-report.md.
CREATE INDEX CONCURRENTLY IF NOT EXISTS perf_regressions_resolved_at_idx
    ON perf_regressions (resolved_at)
    WHERE status = 'resolved' AND resolved_at IS NOT NULL;
