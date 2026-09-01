-- DROP INDEX CONCURRENTLY — вне транзакционного блока, один оператор на
-- файл, тот же приём, что у 0031.
DROP INDEX CONCURRENTLY IF EXISTS issues_project_first_seen_idx;
