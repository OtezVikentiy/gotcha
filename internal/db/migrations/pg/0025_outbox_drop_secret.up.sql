-- backward-compatible: no  (секрет канала вычищен из notification_outbox.payload — старый бинарь берёт его оттуда и доставка встанет)
-- Секреты каналов лежали в notification_outbox.payload (jsonb) — SELECT payload->>'secret'
-- отдавал их за всё окно хранения очереди; новый код резолвит секрет по channel_id при отправке.
-- jsonb_exists(), не оператор `?`: драйвер принимает `?` за плейсхолдер параметра и рвёт соединение.
UPDATE notification_outbox SET payload = payload - 'secret'
WHERE jsonb_exists(payload, 'secret');
