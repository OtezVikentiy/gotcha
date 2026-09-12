-- backward-compatible: yes (новый skip-индекс)
-- trace_id вне ORDER BY — без skip-индекса поиск по /traces/{id} сканирует все проекты.
-- MATERIALIZE не запущен автоматически (не грузить старт тяжёлой мутацией); вручную:
--   ALTER TABLE transactions MATERIALIZE INDEX idx_transactions_trace_id;
ALTER TABLE transactions ADD INDEX IF NOT EXISTS idx_transactions_trace_id trace_id TYPE bloom_filter GRANULARITY 3;
