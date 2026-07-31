-- Откат снимает ограничения и колонку, но НЕ воскрешает висячие членства,
-- удалённые при накатывании: они были следом дефекта, а не данными.
ALTER TABLE team_members DROP CONSTRAINT team_members_member_fk;
ALTER TABLE team_members DROP CONSTRAINT team_members_team_org_fk;
ALTER TABLE team_members ADD CONSTRAINT team_members_team_id_fkey
    FOREIGN KEY (team_id) REFERENCES teams (id) ON DELETE CASCADE;
ALTER TABLE team_members DROP COLUMN org_id;
ALTER TABLE teams DROP CONSTRAINT teams_id_org_key;
