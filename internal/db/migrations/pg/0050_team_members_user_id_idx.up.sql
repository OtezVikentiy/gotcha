-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). user_id NOT NULL — индекс полный.
--
-- team_members несёт ТРИ разных ограничения внешнего ключа без ведущего
-- индекса (этот файл и 0051-0052): team_members_user_id_fkey (user_id) →
-- users(id), team_members_team_org_fk (team_id, org_id) → teams(id, org_id),
-- team_members_member_fk (org_id, user_id) → org_members(org_id, user_id).
-- Три разных ведущих столбца/порядка — три разных индекса, один не
-- покрывает другой (единственный существующий индекс, первичный ключ
-- (team_id, user_id), не ведёт ни одним из них).
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_members_user_id_idx
    ON team_members (user_id);
