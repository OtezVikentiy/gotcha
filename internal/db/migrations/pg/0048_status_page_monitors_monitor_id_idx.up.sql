-- backward-compatible: yes (новый индекс)
--
-- PK (status_page_id, monitor_id) ведёт с status_page_id и не покрывает поиск по monitor_id.
CREATE INDEX CONCURRENTLY IF NOT EXISTS status_page_monitors_monitor_id_idx
    ON status_page_monitors (monitor_id);
