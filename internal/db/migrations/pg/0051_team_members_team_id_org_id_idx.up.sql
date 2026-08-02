-- backward-compatible: yes (новый индекс)
--
-- Находка №45, второй из трёх индексов team_members — см. докблок 0050 про
-- то, почему их три. Составной ключ team_members_team_org_fk ссылается на
-- teams(id, org_id) через FOREIGN KEY (team_id, org_id) — индекс строится по
-- той же последовательности колонок, что и в ограничении: сначала team_id,
-- затем org_id. Обе колонки NOT NULL — индекс полный.
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_members_team_id_org_id_idx
    ON team_members (team_id, org_id);
