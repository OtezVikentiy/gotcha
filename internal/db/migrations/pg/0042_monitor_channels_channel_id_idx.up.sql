-- backward-compatible: yes (новый индекс)
--
-- PK (monitor_id, channel_id) ведёт с monitor_id и не покрывает поиск по channel_id.
CREATE INDEX CONCURRENTLY IF NOT EXISTS monitor_channels_channel_id_idx
    ON monitor_channels (channel_id);
