-- backward-compatible: no

-- Цель ссылки для составного FK: teams(id) уже PRIMARY KEY, но ссылаться надо
-- на пару, иначе участника можно приписать к команде чужой организации.
ALTER TABLE teams ADD CONSTRAINT teams_id_org_key UNIQUE (id, org_id);

ALTER TABLE team_members ADD COLUMN org_id bigint;

UPDATE team_members tm
SET org_id = t.org_id
FROM teams t
WHERE t.id = tm.team_id;

-- Лечение следов дефекта на работающих установках: строки участников, которых
-- уже исключили из организации, но которые до сих пор дают доступ к проектам
-- её команд. Без этой чистки ограничение ниже не установится.
DO $$
DECLARE
    orphans bigint;
BEGIN
    DELETE FROM team_members tm
    WHERE NOT EXISTS (
        SELECT 1 FROM org_members m
        WHERE m.org_id = tm.org_id AND m.user_id = tm.user_id);
    GET DIAGNOSTICS orphans = ROW_COUNT;
    RAISE NOTICE 'team_members: removed % orphaned membership(s)', orphans;
END $$;

ALTER TABLE team_members ALTER COLUMN org_id SET NOT NULL;

-- Одиночный FK на teams(id) поглощается составным: ON DELETE CASCADE
-- сохраняется, а проверка становится строже.
ALTER TABLE team_members DROP CONSTRAINT team_members_team_id_fkey;

ALTER TABLE team_members ADD CONSTRAINT team_members_team_org_fk
    FOREIGN KEY (team_id, org_id) REFERENCES teams (id, org_id) ON DELETE CASCADE;

-- Ключевое ограничение: членство в команде существует только пока существует
-- членство в организации. Каскад снимает команды при удалении участника — то
-- есть RemoveMember не обязан об этом помнить.
ALTER TABLE team_members ADD CONSTRAINT team_members_member_fk
    FOREIGN KEY (org_id, user_id) REFERENCES org_members (org_id, user_id) ON DELETE CASCADE;
