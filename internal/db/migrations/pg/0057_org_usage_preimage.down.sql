-- backward-compatible: no (DROP COLUMN)
ALTER TABLE org_usage
    DROP COLUMN IF EXISTS events_count_before,
    DROP COLUMN IF EXISTS transactions_count_before,
    DROP COLUMN IF EXISTS metrics_count_before,
    DROP COLUMN IF EXISTS profiles_count_before;
