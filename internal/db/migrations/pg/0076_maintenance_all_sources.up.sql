-- backward-compatible: no (DROP CONSTRAINT maintenance_windows_check)
--
-- Замена CHECK формально смягчает условие, но старый бинарь читает ends_at как
-- гарантированно NOT NULL и увидит NULL у бессрочных окон нового бинаря — fail-closed
-- здесь корректен: старый бинарь должен отказаться стартовать, не молча деградировать.
ALTER TABLE host_incidents      ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE metric_incidents    ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE perf_regressions    ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE profile_regressions ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE slo_incidents       ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;

-- Старый CHECK (0006_uptime.up.sql) объявлен inline без имени — PG дал автоимя
-- maintenance_windows_check (единственный unnamed CHECK на этой таблице).
ALTER TABLE maintenance_windows DROP CONSTRAINT maintenance_windows_check;
ALTER TABLE maintenance_windows ADD CONSTRAINT maintenance_windows_shape CHECK (
  (weekly AND weekday IS NOT NULL AND start_time IS NOT NULL AND end_time IS NOT NULL)
  OR
  (NOT weekly AND starts_at IS NOT NULL AND (ends_at IS NULL OR ends_at > starts_at))
);
