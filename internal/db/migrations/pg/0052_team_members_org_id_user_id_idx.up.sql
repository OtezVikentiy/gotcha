-- backward-compatible: yes (новый индекс)
--
-- Порядок колонок повторяет FK team_members_member_fk (org_id, user_id) — см. 0050.
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_members_org_id_user_id_idx
    ON team_members (org_id, user_id);
