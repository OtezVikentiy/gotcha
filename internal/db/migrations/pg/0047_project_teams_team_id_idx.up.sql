-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). team_id NOT NULL — индекс полный. Первичный ключ
-- таблицы (project_id, team_id) ведёт с project_id и не покрывает поиск по
-- team_id при удалении команды.
CREATE INDEX CONCURRENTLY IF NOT EXISTS project_teams_team_id_idx
    ON project_teams (team_id);
