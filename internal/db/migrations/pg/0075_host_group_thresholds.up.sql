-- backward-compatible: yes (новая таблица)
-- Групповые пороги хост-инцидентов по метке (окружение/роль) из B1. Nullable-поля
-- как в 0074. Матч по метке — точная строка (значения из телеметрии).
CREATE TABLE host_group_thresholds (
    project_id           BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    scope                TEXT NOT NULL CHECK (scope IN ('env','role')),
    label                TEXT NOT NULL,
    disk_enabled         BOOLEAN,
    disk_threshold       DOUBLE PRECISION CHECK (disk_threshold IS NULL OR (disk_threshold > 0 AND disk_threshold <= 1)),
    memory_enabled       BOOLEAN,
    memory_threshold     DOUBLE PRECISION CHECK (memory_threshold IS NULL OR (memory_threshold > 0 AND memory_threshold <= 1)),
    load_enabled         BOOLEAN,
    load_threshold       DOUBLE PRECISION CHECK (load_threshold IS NULL OR load_threshold > 0),
    silent_enabled       BOOLEAN,
    silent_after_seconds INTEGER CHECK (silent_after_seconds IS NULL OR silent_after_seconds >= 180),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, scope, label)
);
