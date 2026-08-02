-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). monitor_id NOT NULL — индекс полный. Первичный ключ
-- таблицы (status_page_id, monitor_id) ведёт с status_page_id и не покрывает
-- поиск по monitor_id при удалении монитора.
CREATE INDEX CONCURRENTLY IF NOT EXISTS status_page_monitors_monitor_id_idx
    ON status_page_monitors (monitor_id);
