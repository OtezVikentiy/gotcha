-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md): rule_id NOT NULL — индекс полный.
CREATE INDEX CONCURRENTLY IF NOT EXISTS alert_throttle_rule_id_idx
    ON alert_throttle (rule_id);
