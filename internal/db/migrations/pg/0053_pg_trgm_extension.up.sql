-- backward-compatible: yes (CREATE EXTENSION, тот же приём, что citext в 0001_init)
--
-- pg_trgm даёт GIN-опкласс (gin_trgm_ops, см. 0054/0055) для ILIKE по подстроке,
-- где обычный btree не помогает. Отдельным файлом от индексов: CREATE INDEX
-- CONCURRENTLY запрещён внутри любого транзакционного блока (SQLSTATE 25001, см. 0031),
-- даже открытого предшествующим CREATE EXTENSION в том же ExecContext.
CREATE EXTENSION IF NOT EXISTS pg_trgm;
