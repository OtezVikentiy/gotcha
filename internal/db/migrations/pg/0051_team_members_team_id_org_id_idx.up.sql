-- backward-compatible: yes (новый индекс)
--
-- Порядок колонок повторяет FK team_members_team_org_fk (team_id, org_id) — см. 0050.
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_members_team_id_org_id_idx
    ON team_members (team_id, org_id);
