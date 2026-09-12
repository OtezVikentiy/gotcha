-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS notification_outbox_channel_id_idx
    ON notification_outbox (channel_id);
