-- backward-compatible: yes (новая таблица)
-- Переопределение порогов хост-инцидентов для КОНКРЕТНОГО хоста (B2). Все поля
-- nullable: NULL = наследовать от группы/проекта/дефолта. Вид «задан» на этом
-- уровне, если <kind>_enabled IS NOT NULL.
CREATE TABLE host_threshold_overrides (
    host_id              BIGINT PRIMARY KEY REFERENCES hosts(id) ON DELETE CASCADE,
    disk_enabled         BOOLEAN,
    disk_threshold       DOUBLE PRECISION CHECK (disk_threshold IS NULL OR (disk_threshold > 0 AND disk_threshold <= 1)),
    memory_enabled       BOOLEAN,
    memory_threshold     DOUBLE PRECISION CHECK (memory_threshold IS NULL OR (memory_threshold > 0 AND memory_threshold <= 1)),
    load_enabled         BOOLEAN,
    load_threshold       DOUBLE PRECISION CHECK (load_threshold IS NULL OR load_threshold > 0),
    silent_enabled       BOOLEAN,
    silent_after_seconds INTEGER CHECK (silent_after_seconds IS NULL OR silent_after_seconds >= 180),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
