-- backward-compatible: yes (ADD COLUMN с дефолтами)
-- Квота профилей — per-request счётчик, как у метрик.
ALTER TABLE organizations ADD COLUMN profile_quota bigint NOT NULL DEFAULT 1000000;
ALTER TABLE org_usage ADD COLUMN profiles_count bigint NOT NULL DEFAULT 0;
