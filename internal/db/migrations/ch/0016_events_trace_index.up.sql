-- backward-compatible: yes (новый skip-индекс)
-- trace_id/event_id вне ORDER BY (project_id, issue_id, timestamp) — без skip-индекса
-- поиск по ним сканирует все события проекта. Про MATERIALIZE — см. 0015.
ALTER TABLE events ADD INDEX IF NOT EXISTS idx_events_trace_id trace_id TYPE bloom_filter GRANULARITY 3, ADD INDEX IF NOT EXISTS idx_events_event_id event_id TYPE bloom_filter GRANULARITY 3;
