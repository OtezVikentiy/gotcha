DROP TABLE metric_incidents;
DROP TABLE metric_alert_rules;
ALTER TABLE org_usage DROP COLUMN metrics_count;
ALTER TABLE organizations DROP COLUMN metric_quota;
