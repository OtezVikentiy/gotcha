-- Возврат legacy-дефолтов колонок квот. Значения строк не восстанавливаем.
ALTER TABLE organizations ALTER COLUMN transaction_quota SET DEFAULT 100000;
ALTER TABLE organizations ALTER COLUMN metric_quota SET DEFAULT 1000000;
ALTER TABLE organizations ALTER COLUMN profile_quota SET DEFAULT 1000000;
