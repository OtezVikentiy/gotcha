-- backward-compatible: yes (новый индекс)
--
-- Находка №5: entityRules чистит incidents по resolved_at с условием
-- закрытости "resolved_at IS NOT NULL" (internal/telemetry/entity_janitor.go)
-- — открытый инцидент описывает то, что происходит сейчас, и удалять его по
-- возрасту нельзя. Индекс частичный: строится только по строкам с этим же
-- условием, поэтому меньше полного и совпадает с запросом планировщика
-- дословно — сверено с entityRules, а не с планом задачи (план мог
-- устареть).
--
-- Один индекс на файл — вывод эксперимента, см. докблок
-- 0031_issues_last_seen_idx.up.sql и task-1-report.md.
CREATE INDEX CONCURRENTLY IF NOT EXISTS incidents_resolved_at_idx
    ON incidents (resolved_at) WHERE resolved_at IS NOT NULL;
