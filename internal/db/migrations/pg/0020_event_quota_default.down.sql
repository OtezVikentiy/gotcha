-- Возврат legacy-дефолта колонки event_quota. Значения строк не восстанавливаем.
ALTER TABLE organizations ALTER COLUMN event_quota SET DEFAULT 1000000;
