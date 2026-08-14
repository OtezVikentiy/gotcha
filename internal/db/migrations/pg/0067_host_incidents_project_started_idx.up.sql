-- backward-compatible: yes (новый индекс)
--
-- Покрывает ListByProject: инциденты проекта, свежайшие первыми (ORDER BY
-- started_at DESC) — для UI карточки хоста/проекта.
CREATE INDEX CONCURRENTLY IF NOT EXISTS host_incidents_project_started_idx
    ON host_incidents (project_id, started_at DESC);
