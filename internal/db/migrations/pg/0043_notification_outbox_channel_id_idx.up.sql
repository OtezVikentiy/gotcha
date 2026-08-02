-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). channel_id NOT NULL — индекс полный.
CREATE INDEX CONCURRENTLY IF NOT EXISTS notification_outbox_channel_id_idx
    ON notification_outbox (channel_id);
