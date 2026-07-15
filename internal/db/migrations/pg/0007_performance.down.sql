ALTER TABLE organizations DROP COLUMN IF EXISTS transaction_quota;

ALTER TABLE projects DROP COLUMN IF EXISTS perf_detector_config;
ALTER TABLE projects DROP COLUMN IF EXISTS apdex_threshold_ms;
ALTER TABLE projects DROP COLUMN IF EXISTS transaction_sample_rate;

DROP TABLE IF EXISTS perf_issues;
