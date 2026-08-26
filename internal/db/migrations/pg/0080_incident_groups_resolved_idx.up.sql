-- backward-compatible: yes (новый частичный индекс)
-- R2b/W10: PurgeOldGroups (janitor.go) фильтрует по resolved_at IS NOT NULL
-- AND resolved_at < cutoff; incident_groups_open_idx (0079) — партиал на
-- ПРОТИВОПОЛОЖНЫЙ предикат (resolved_at IS NULL), под этот запрос индекса нет.
CREATE INDEX incident_groups_resolved_idx ON incident_groups(resolved_at) WHERE resolved_at IS NOT NULL;
