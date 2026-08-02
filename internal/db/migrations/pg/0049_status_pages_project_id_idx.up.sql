-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). project_id NOT NULL — индекс полный.
CREATE INDEX CONCURRENTLY IF NOT EXISTS status_pages_project_id_idx
    ON status_pages (project_id);
