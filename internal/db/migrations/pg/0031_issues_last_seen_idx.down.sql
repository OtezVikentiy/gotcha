-- DROP INDEX CONCURRENTLY — тоже выполняется вне транзакционного блока и
-- подчиняется тому же выводу эксперимента (task-1-report.md): один оператор
-- на файл.
DROP INDEX CONCURRENTLY IF EXISTS issues_last_seen_idx;
