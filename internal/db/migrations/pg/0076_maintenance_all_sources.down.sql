-- Сначала снять новый CHECK, удалить бессрочные окна (откат их теряет — dev-only
-- приемлемо, иначе ADD CONSTRAINT строгого CHECK немедленно валидирует существующие
-- строки и падает на ends_at NULL), потом вернуть строгий CHECK 0006.
ALTER TABLE maintenance_windows DROP CONSTRAINT maintenance_windows_shape;
DELETE FROM maintenance_windows WHERE NOT weekly AND ends_at IS NULL;
ALTER TABLE maintenance_windows ADD CONSTRAINT maintenance_windows_check CHECK (
  (weekly AND weekday IS NOT NULL AND start_time IS NOT NULL AND end_time IS NOT NULL)
  OR
  (NOT weekly AND starts_at IS NOT NULL AND ends_at IS NOT NULL AND ends_at > starts_at)
);

ALTER TABLE host_incidents      DROP COLUMN in_maintenance;
ALTER TABLE metric_incidents    DROP COLUMN in_maintenance;
ALTER TABLE perf_regressions    DROP COLUMN in_maintenance;
ALTER TABLE profile_regressions DROP COLUMN in_maintenance;
ALTER TABLE slo_incidents       DROP COLUMN in_maintenance;
