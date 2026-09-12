-- backward-compatible: yes (ADD COLUMN с дефолтом)
-- Флаг в профиле, а не в cookie: скрытое не должно воскресать на другом устройстве.
ALTER TABLE users ADD COLUMN hide_getting_started boolean NOT NULL DEFAULT false;
