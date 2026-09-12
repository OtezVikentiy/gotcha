-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS org_invites_org_id_idx
    ON org_invites (org_id);
