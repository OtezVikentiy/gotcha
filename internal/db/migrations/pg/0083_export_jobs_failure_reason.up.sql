-- backward-compatible: yes (аддитивно — новая nullable-колонка)
-- Причина отказа раньше жила только в памяти воркера на время письма — без почты
-- автор её не видел. NULL — не терминальна или строка старше миграции.
ALTER TABLE export_jobs ADD COLUMN failure_reason_key text;
