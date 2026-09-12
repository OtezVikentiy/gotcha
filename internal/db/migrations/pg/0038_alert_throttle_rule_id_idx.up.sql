-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS alert_throttle_rule_id_idx
    ON alert_throttle (rule_id);
