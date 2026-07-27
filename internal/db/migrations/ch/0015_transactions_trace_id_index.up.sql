-- Skip-индекс bloom_filter на transactions.trace_id. ProjectForTrace и страница
-- /traces/{id} ищут транзакцию по trace_id, которого нет в первичном ключе
-- (ORDER BY project_id, transaction, timestamp) — без индекса это полный скан по
-- всем проектам. Индекс применяется к НОВЫМ кускам; на уже записанных данных
-- вступает в силу по мере слияний, либо вручную одноразово:
--   ALTER TABLE transactions MATERIALIZE INDEX idx_transactions_trace_id;
-- MATERIALIZE намеренно НЕ включён в миграцию, чтобы старт приложения не запускал
-- тяжёлую фоновую мутацию по всей таблице на больших инстансах.
ALTER TABLE transactions ADD INDEX IF NOT EXISTS idx_transactions_trace_id trace_id TYPE bloom_filter GRANULARITY 3;
