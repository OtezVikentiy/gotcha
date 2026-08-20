-- Партиционные индексы падают вместе с колонками, отдельный DROP INDEX не нужен.
-- Новые таблицы — в обратном порядке FK-зависимостей.
DROP TABLE incident_escalations;
DROP TABLE escalation_step_channels;
DROP TABLE escalation_steps;

ALTER TABLE metric_alert_rules DROP COLUMN severity;

ALTER TABLE host_incidents      DROP COLUMN acknowledged_at;
ALTER TABLE host_incidents      DROP COLUMN acknowledged_by;
ALTER TABLE host_incidents      DROP COLUMN severity;
ALTER TABLE host_incidents      DROP COLUMN escalation_level;
ALTER TABLE host_incidents      DROP COLUMN last_escalated_at;

ALTER TABLE metric_incidents    DROP COLUMN acknowledged_at;
ALTER TABLE metric_incidents    DROP COLUMN acknowledged_by;
ALTER TABLE metric_incidents    DROP COLUMN severity;
ALTER TABLE metric_incidents    DROP COLUMN escalation_level;
ALTER TABLE metric_incidents    DROP COLUMN last_escalated_at;

ALTER TABLE perf_regressions    DROP COLUMN acknowledged_at;
ALTER TABLE perf_regressions    DROP COLUMN acknowledged_by;
ALTER TABLE perf_regressions    DROP COLUMN severity;
ALTER TABLE perf_regressions    DROP COLUMN escalation_level;
ALTER TABLE perf_regressions    DROP COLUMN last_escalated_at;

ALTER TABLE profile_regressions DROP COLUMN acknowledged_at;
ALTER TABLE profile_regressions DROP COLUMN acknowledged_by;
ALTER TABLE profile_regressions DROP COLUMN severity;
ALTER TABLE profile_regressions DROP COLUMN escalation_level;
ALTER TABLE profile_regressions DROP COLUMN last_escalated_at;

ALTER TABLE slo_incidents       DROP COLUMN acknowledged_at;
ALTER TABLE slo_incidents       DROP COLUMN acknowledged_by;
ALTER TABLE slo_incidents       DROP COLUMN severity;
ALTER TABLE slo_incidents       DROP COLUMN escalation_level;
ALTER TABLE slo_incidents       DROP COLUMN last_escalated_at;
