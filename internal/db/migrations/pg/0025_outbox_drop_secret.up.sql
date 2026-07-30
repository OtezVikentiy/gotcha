-- backward-compatible: no  (секрет канала вычищен из notification_outbox.payload — старый бинарь берёт его оттуда и доставка встанет)
-- Вычищаем расшифрованные секреты каналов из уже накопленных задач очереди.
-- Раньше alert.Evaluator и uptime.OutboxNotifier клали bot-токен Telegram и
-- HMAC-ключ вебхука прямо в notification_outbox.payload — обычный jsonb, — и
-- шифрование alert_channels.secret этим обесценивалось полностью: запрос
-- `SELECT payload->>'secret' FROM notification_outbox` отдавал живые секреты
-- за всё окно хранения очереди (janitor чистит только sent/failed старше 7
-- дней, так что окно скользящее и не закрывается никогда).
--
-- Новый код секрет в payload не кладёт: notify.Worker достаёт его по
-- channel_id в момент отправки (см. notify.SecretResolver). Эта миграция
-- закрывает уже утёкшее — и она же чинит задачи, поставленные старой
-- версией: секрет им больше не нужен, доставка резолвит его сама.
--
-- Условие написано через jsonb_exists(), а не через оператор `?`: драйвер
-- принимает знак вопроса за плейсхолдер параметра и рвёт соединение на этой
-- миграции ("unexpected EOF"). Функциональная форма делает ровно то же.
UPDATE notification_outbox SET payload = payload - 'secret'
WHERE jsonb_exists(payload, 'secret');
