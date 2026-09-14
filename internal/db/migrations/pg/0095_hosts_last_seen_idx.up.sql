-- backward-compatible: yes (новый индекс)
--
-- CREATE INDEX CONCURRENTLY: один оператор на файл, вне транзакции (иначе SQLSTATE 25001,
-- golang-migrate шлёт файл одним ExecContext). Обрыв построения оставляет невалидный индекс —
-- IF NOT EXISTS его молча не чинит: DROP INDEX CONCURRENTLY <имя> и повторный накат.
-- Тот же приём — во всех индексах 0031-0036, 0089.
CREATE INDEX CONCURRENTLY IF NOT EXISTS hosts_last_seen_idx
    ON hosts (last_seen);
