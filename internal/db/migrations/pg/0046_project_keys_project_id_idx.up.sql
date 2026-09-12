-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS project_keys_project_id_idx
    ON project_keys (project_id);
