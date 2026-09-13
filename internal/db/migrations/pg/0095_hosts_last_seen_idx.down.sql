-- DROP INDEX CONCURRENTLY — тоже вне транзакционного блока, один оператор на файл.
DROP INDEX CONCURRENTLY IF EXISTS hosts_last_seen_idx;
