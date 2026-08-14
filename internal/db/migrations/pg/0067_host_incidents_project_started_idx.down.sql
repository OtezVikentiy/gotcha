-- DROP INDEX CONCURRENTLY вне транзакции, один оператор на файл — см. 0031.
DROP INDEX CONCURRENTLY IF EXISTS host_incidents_project_started_idx;
