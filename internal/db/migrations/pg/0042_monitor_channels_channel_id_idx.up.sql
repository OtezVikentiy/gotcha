-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). channel_id NOT NULL — индекс полный.
-- Первичный ключ таблицы (monitor_id, channel_id) ведёт с monitor_id и не
-- покрывает поиск по channel_id при удалении канала уведомлений.
CREATE INDEX CONCURRENTLY IF NOT EXISTS monitor_channels_channel_id_idx
    ON monitor_channels (channel_id);
