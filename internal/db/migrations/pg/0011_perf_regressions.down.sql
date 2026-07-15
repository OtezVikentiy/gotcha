ALTER TABLE projects DROP COLUMN IF EXISTS perf_regression_config;

-- DROP TABLE уносит и оба индекса (one_open, project_started).
DROP TABLE IF EXISTS perf_regressions;
