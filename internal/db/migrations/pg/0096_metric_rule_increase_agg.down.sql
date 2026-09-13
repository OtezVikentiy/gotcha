-- Старый бинарь не умеет increase — переводим на sum, чтобы ADD CONSTRAINT не упал
-- на живых правилах; семантика при этом теряется (снова средняя скорость, не прирост).
UPDATE metric_alert_rules SET aggregation = 'sum' WHERE aggregation = 'increase';

ALTER TABLE metric_alert_rules DROP CONSTRAINT metric_alert_rules_aggregation_check;
ALTER TABLE metric_alert_rules ADD CONSTRAINT metric_alert_rules_aggregation_check
  CHECK (aggregation IN ('avg','max','min','sum','p50','p95','p99'));
