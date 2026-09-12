-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS host_incidents_project_started_idx
    ON host_incidents (project_id, started_at DESC);
