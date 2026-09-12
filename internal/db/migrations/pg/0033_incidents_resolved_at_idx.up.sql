-- backward-compatible: yes (новый индекс)
--
-- Открытый инцидент описывает текущее состояние — чистильщику (entity_janitor.go)
-- удалять его по возрасту нельзя, индекс частичный под то же условие. Приём CONCURRENTLY — см. 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS incidents_resolved_at_idx
    ON incidents (resolved_at) WHERE resolved_at IS NOT NULL;
