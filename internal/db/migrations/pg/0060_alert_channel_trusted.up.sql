-- backward-compatible: yes (ADD COLUMN с дефолтом)
-- Для Telegram chat_id не домен — канал всегда числится внешним. Флаг открывает
-- детали поштучно вместо глобального GOTCHA_EXTERNAL_CHANNEL_DETAILS_ENABLED.
ALTER TABLE alert_channels ADD COLUMN trusted boolean NOT NULL DEFAULT false;
