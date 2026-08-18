ALTER TABLE org_usage
    DROP COLUMN IF EXISTS logs_count,
    DROP COLUMN IF EXISTS logs_count_before,
    DROP COLUMN IF EXISTS dropped_logs;
ALTER TABLE organizations
    DROP COLUMN IF EXISTS log_quota;
