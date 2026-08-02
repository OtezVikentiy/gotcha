-- DROP INDEX CONCURRENTLY вне транзакции, один оператор на файл — см. 0031.
DROP INDEX CONCURRENTLY IF EXISTS project_keys_project_id_idx;
