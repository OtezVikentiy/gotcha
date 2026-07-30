-- backward-compatible: yes (новый skip-индекс)
-- Skip-индекс на profile_samples.trace_id (HasProfileForTrace/FlameForTrace —
-- запускаются на рендере waterfall). trace_id добавлен колонкой (0011) и в
-- первичный ключ (ORDER BY project_id, profile_type, service, ts) не входит.
-- Про MATERIALIZE — см. 0015. Таблица живёт 7 дней, так что индекс покрывает
-- весь актуальный объём быстро.
ALTER TABLE profile_samples ADD INDEX IF NOT EXISTS idx_profile_samples_trace_id trace_id TYPE bloom_filter GRANULARITY 3;
