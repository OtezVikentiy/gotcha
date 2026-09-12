-- backward-compatible: yes (новый индекс)
--
-- PK (org_id, user_id) ведёт с org_id и не покрывает поиск по user_id.
CREATE INDEX CONCURRENTLY IF NOT EXISTS org_members_user_id_idx
    ON org_members (user_id);
