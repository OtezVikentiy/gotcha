-- backward-compatible: yes (новый индекс)
--
-- issues_project_last_seen_idx (0003) продолжается last_seen, не first_seen —
-- planner не может отсечь по нему WHERE project_id=$1 AND first_seen>=$2. Приём CONCURRENTLY — см. 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS issues_project_first_seen_idx
    ON issues (project_id, first_seen);
