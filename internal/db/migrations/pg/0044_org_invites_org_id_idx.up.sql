-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). org_id NOT NULL — индекс полный.
CREATE INDEX CONCURRENTLY IF NOT EXISTS org_invites_org_id_idx
    ON org_invites (org_id);
