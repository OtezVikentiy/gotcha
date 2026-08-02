-- DROP INDEX CONCURRENTLY вне транзакции, один оператор на файл — см. 0031.
DROP INDEX CONCURRENTLY IF EXISTS status_page_monitors_monitor_id_idx;
