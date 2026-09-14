-- backward-compatible: no (старый бинарь молча читает increase-правило как rate)
--
-- Автоимя из 0013_metrics.up.sql (unnamed inline CHECK) — единственный CHECK
-- на этой колонке, поэтому имя стабильно.
ALTER TABLE metric_alert_rules DROP CONSTRAINT metric_alert_rules_aggregation_check;
ALTER TABLE metric_alert_rules ADD CONSTRAINT metric_alert_rules_aggregation_check
  CHECK (aggregation IN ('avg','max','min','sum','increase','p50','p95','p99'));
