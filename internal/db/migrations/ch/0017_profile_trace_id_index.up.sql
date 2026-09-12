-- backward-compatible: yes (новый skip-индекс)
-- trace_id добавлен колонкой (0011) и вне ORDER BY — без skip-индекса поиск по нему
-- сканирует таблицу. Про MATERIALIZE — см. 0015.
ALTER TABLE profile_samples ADD INDEX IF NOT EXISTS idx_profile_samples_trace_id trace_id TYPE bloom_filter GRANULARITY 3;
