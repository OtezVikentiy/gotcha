-- backward-compatible: no (ALTER COLUMN ... SET NOT NULL: разрушительная
-- форма по guard'у internal/guards/migrations_test.go destructiveSQL, откат
-- релиза на бинарь СО СХЕМОЙ до этой миграции запрещён гейтом схемы. На
-- практике ни один путь записи не страдает — internal/host/incident.go
-- Open/Bump/Resolve и так принимают current/peak как обязательный float64 и
-- NULL не оставляют, — но маркер отражает форму DDL, а не то, что конкретно
-- этот продовый писатель уже гарантирует).
--
-- 0066 создала host_incidents.current_value/peak_value как double precision
-- без NOT NULL — расхождение со схемой в Go: Incident.CurrentValue/PeakValue
-- (internal/host/incident.go) — обычные float64, scanIncident сканирует их
-- напрямую, без sql.NullFloat64/COALESCE. Строка с NULL в любой из колонок
-- (например, вставленная вручную SQL мимо IncidentService) роняет чтение
-- ошибкой Scan, а не отдаёт нулевое значение.
--
-- UPDATE перед ALTER — страховка на случай уже существующих NULL-строк на
-- работающих установках (ручные вставки, будущие бэкапы/восстановления):
-- без неё ALTER ... SET NOT NULL отказал бы прямо здесь.
UPDATE host_incidents SET current_value = 0 WHERE current_value IS NULL;
UPDATE host_incidents SET peak_value = 0 WHERE peak_value IS NULL;

ALTER TABLE host_incidents ALTER COLUMN current_value SET NOT NULL;
ALTER TABLE host_incidents ALTER COLUMN peak_value SET NOT NULL;
