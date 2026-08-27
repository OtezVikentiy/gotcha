-- Партиционные индексы падают вместе с колонками, отдельный DROP INDEX не нужен
-- (incidents_esc_pending_idx — исключение, он не привязан к дропаемой колонке).
DROP INDEX incidents_esc_pending_idx;

ALTER TABLE incidents DROP COLUMN notify_open_failed;
ALTER TABLE incidents DROP COLUMN notify_open_attempts;

ALTER TABLE incidents DROP COLUMN acknowledged_at;
ALTER TABLE incidents DROP COLUMN acknowledged_by;
ALTER TABLE incidents DROP COLUMN severity;
ALTER TABLE incidents DROP COLUMN escalation_level;
ALTER TABLE incidents DROP COLUMN last_escalated_at;
