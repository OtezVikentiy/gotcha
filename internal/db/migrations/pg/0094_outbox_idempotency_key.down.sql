DROP INDEX IF EXISTS notification_outbox_idempotency_key_idx;
ALTER TABLE notification_outbox DROP COLUMN IF EXISTS idempotency_key;
