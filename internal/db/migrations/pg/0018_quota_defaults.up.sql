-- backward-compatible: yes (смена значений по умолчанию)
-- DEFAULT квот -> 0 (безлимит); сбрасываются только строки, ещё держащие legacy-дефолт
-- (WHERE = старое значение) — операторские правки не трогаем.
ALTER TABLE organizations ALTER COLUMN transaction_quota SET DEFAULT 0;
ALTER TABLE organizations ALTER COLUMN metric_quota SET DEFAULT 0;
ALTER TABLE organizations ALTER COLUMN profile_quota SET DEFAULT 0;
UPDATE organizations SET transaction_quota = 0 WHERE transaction_quota = 100000;
UPDATE organizations SET metric_quota = 0 WHERE metric_quota = 1000000;
UPDATE organizations SET profile_quota = 0 WHERE profile_quota = 1000000;
