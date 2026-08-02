-- DROP INDEX CONCURRENTLY вне транзакции, один оператор на файл — см. 0031.
DROP INDEX CONCURRENTLY IF EXISTS check_queue_leased_by_idx;
