-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS status_pages_project_id_idx
    ON status_pages (project_id);
