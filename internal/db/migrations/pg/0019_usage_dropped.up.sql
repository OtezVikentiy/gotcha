-- backward-compatible: yes (ADD COLUMN с дефолтом)
-- Счётчики отклонённых (drop) единиц — отдельные от принятых, инкрементируются
-- независимо (org.IncDropped*).
ALTER TABLE org_usage ADD COLUMN dropped_events bigint NOT NULL DEFAULT 0;
ALTER TABLE org_usage ADD COLUMN dropped_transactions bigint NOT NULL DEFAULT 0;
ALTER TABLE org_usage ADD COLUMN dropped_metrics bigint NOT NULL DEFAULT 0;
ALTER TABLE org_usage ADD COLUMN dropped_profiles bigint NOT NULL DEFAULT 0;
