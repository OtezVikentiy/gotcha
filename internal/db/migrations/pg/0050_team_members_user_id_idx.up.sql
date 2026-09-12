-- backward-compatible: yes (новый индекс)
--
-- team_members несёт три разных FK без ведущего индекса (этот файл и 0051-0052) —
-- три разных ведущих столбца, PK (team_id, user_id) не ведёт ни одним из них.
CREATE INDEX CONCURRENTLY IF NOT EXISTS team_members_user_id_idx
    ON team_members (user_id);
