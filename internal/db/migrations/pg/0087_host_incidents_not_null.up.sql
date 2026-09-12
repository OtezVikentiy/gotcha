-- backward-compatible: no (SET NOT NULL — разрушительная форма гейта схемы)
--
-- current_value/peak_value без NOT NULL расходились с Go float64 (incident.go,
-- scanIncident без sql.NullFloat64) — NULL-строка роняет чтение ошибкой Scan.
UPDATE host_incidents SET current_value = 0 WHERE current_value IS NULL;
UPDATE host_incidents SET peak_value = 0 WHERE peak_value IS NULL;

ALTER TABLE host_incidents ALTER COLUMN current_value SET NOT NULL;
ALTER TABLE host_incidents ALTER COLUMN peak_value SET NOT NULL;
