-- backward-compatible: yes (аддитивно; колонка NULL у всех существующих строк
-- и у источников, не передающих ключ — частичный индекс уникальность не
-- проверяет NULL, конфликтовать с текущими данными нечему).
ALTER TABLE notification_outbox ADD COLUMN idempotency_key text;
CREATE UNIQUE INDEX notification_outbox_idempotency_key_idx
    ON notification_outbox (idempotency_key) WHERE idempotency_key IS NOT NULL;
