-- backward-compatible: yes (новый индекс)
--
-- PK (project_id, team_id) ведёт с project_id и не покрывает поиск по team_id.
CREATE INDEX CONCURRENTLY IF NOT EXISTS project_teams_team_id_idx
    ON project_teams (team_id);
