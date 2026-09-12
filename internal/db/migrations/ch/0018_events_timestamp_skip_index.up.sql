-- backward-compatible: yes (новый skip-индекс)
-- ORDER BY (project_id, issue_id, timestamp) не отсекает по времени без ограничения issue_id —
-- CountsSince (детектор всплесков) группирует по issue_id за окно и читает всю партицию-месяц.
-- Не материализуем автоматически (события живут TTL 90 дней, куски сменятся ротацией);
-- вручную: ALTER TABLE events MATERIALIZE INDEX idx_events_timestamp.
ALTER TABLE events ADD INDEX IF NOT EXISTS idx_events_timestamp timestamp TYPE minmax GRANULARITY 4;
