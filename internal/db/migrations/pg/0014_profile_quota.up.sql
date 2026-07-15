-- Этап 7 (профилирование): квота профилей (per-request счётчик, как метрики).
ALTER TABLE organizations ADD COLUMN profile_quota bigint NOT NULL DEFAULT 1000000;
ALTER TABLE org_usage ADD COLUMN profiles_count bigint NOT NULL DEFAULT 0;
