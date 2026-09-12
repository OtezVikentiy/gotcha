-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS issues_assignee_id_idx
    ON issues (assignee_id) WHERE assignee_id IS NOT NULL;
