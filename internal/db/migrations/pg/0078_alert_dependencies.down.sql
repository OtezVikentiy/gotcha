ALTER TABLE incidents      DROP COLUMN suppressed_by_dep;
ALTER TABLE host_incidents DROP COLUMN suppressed_by_dep;
DROP TABLE alert_dependencies;
