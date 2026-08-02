-- DROP INDEX CONCURRENTLY вне транзакции, один оператор на файл — см. 0031.
DROP INDEX CONCURRENTLY IF EXISTS team_members_user_id_idx;
