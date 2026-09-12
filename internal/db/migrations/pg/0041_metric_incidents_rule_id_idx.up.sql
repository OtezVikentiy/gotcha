-- backward-compatible: yes (новый индекс)
--
-- metric_incidents_one_open_idx ведёт с rule_id, но партиционный (WHERE status='open') —
-- закрытые инциденты под этим условием не лежат, каскад для них всё равно сканил бы.
CREATE INDEX CONCURRENTLY IF NOT EXISTS metric_incidents_rule_id_idx
    ON metric_incidents (rule_id);
