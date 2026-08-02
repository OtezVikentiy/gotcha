-- backward-compatible: yes (новый индекс)
--
-- Находка №45, третий из трёх индексов team_members — см. докблок 0050 про
-- то, почему их три. Составной ключ team_members_member_fk ссылается на
-- org_members(org_id, user_id) через FOREIGN KEY (org_id, user_id) — индекс
-- строится по той же последовательности колонок: сначала org_id, затем
-- user_id. Обе колонки NOT NULL — индекс полный.
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_members_org_id_user_id_idx
    ON team_members (org_id, user_id);
