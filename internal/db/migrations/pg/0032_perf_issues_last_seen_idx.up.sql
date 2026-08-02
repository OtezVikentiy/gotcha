-- backward-compatible: yes (новый индекс)
--
-- Находка №5, тот же дефект и то же обоснование, что у 0031, для соседней
-- таблицы: entityRules чистит perf_issues по last_seen, а
-- perf_issues_project_last_seen_idx начинается с project_id и не помогает
-- скану по возрасту всей таблицы. Один индекс на файл — вывод эксперимента,
-- см. докблок 0031_issues_last_seen_idx.up.sql и task-1-report.md.
CREATE INDEX CONCURRENTLY IF NOT EXISTS perf_issues_last_seen_idx
    ON perf_issues (last_seen);
