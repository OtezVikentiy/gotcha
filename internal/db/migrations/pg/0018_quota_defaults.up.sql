-- PROD-B2: безлимит квот по умолчанию (OSS). Меняем DEFAULT колонок квот на 0
-- и сбрасываем в 0 существующие строки, всё ещё держащие legacy-хардкод-дефолты
-- (transaction_quota=100000, metric/profile=1000000). Явно заданные оператором
-- иные значения не трогаются (WHERE = старый дефолт).
ALTER TABLE organizations ALTER COLUMN transaction_quota SET DEFAULT 0;
ALTER TABLE organizations ALTER COLUMN metric_quota SET DEFAULT 0;
ALTER TABLE organizations ALTER COLUMN profile_quota SET DEFAULT 0;
UPDATE organizations SET transaction_quota = 0 WHERE transaction_quota = 100000;
UPDATE organizations SET metric_quota = 0 WHERE metric_quota = 1000000;
UPDATE organizations SET profile_quota = 0 WHERE profile_quota = 1000000;
