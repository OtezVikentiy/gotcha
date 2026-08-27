-- Дубли, схлопнутые up-миграцией, не воскрешаются — см. её комментарий:
-- какая именно строка была "лишней" копией, после DELETE не восстановить.
DROP TABLE escalation_step_log_failures;

ALTER TABLE incident_escalations
    DROP CONSTRAINT incident_escalations_source_incident_channel_step_key;
