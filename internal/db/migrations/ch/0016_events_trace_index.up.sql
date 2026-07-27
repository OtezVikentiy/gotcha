-- Skip-индексы на events.trace_id (ByTraceID — waterfall) и events.event_id
-- (EventByID — глубокие ?event=-ссылки). Оба вне первичного ключа (ORDER BY
-- project_id, issue_id, timestamp), поэтому поиск сканирует все события проекта.
-- Про MATERIALIZE и поведение на старых кусках — см. 0015.
ALTER TABLE events ADD INDEX IF NOT EXISTS idx_events_trace_id trace_id TYPE bloom_filter GRANULARITY 3, ADD INDEX IF NOT EXISTS idx_events_event_id event_id TYPE bloom_filter GRANULARITY 3;
