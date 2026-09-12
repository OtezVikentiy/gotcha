-- backward-compatible: yes (UPDATE только текстового столбца, схема не меняется)
-- last_error нёс сырые секреты доставки (email эхал адрес получателя, webhook — тело ответа
-- с токеном в URL цели) — оба источника зачинены, здесь разово чистим уже накопленные строки.
UPDATE notification_outbox SET last_error = '[redacted on upgrade]' WHERE last_error <> '';
