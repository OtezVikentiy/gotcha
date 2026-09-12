-- backward-compatible: yes (смена значения по умолчанию)
-- Тот же приём, что 0018: DEFAULT event_quota -> 0, сбрасываются только строки
-- с legacy-дефолтом (WHERE = старое значение).
ALTER TABLE organizations ALTER COLUMN event_quota SET DEFAULT 0;
UPDATE organizations SET event_quota = 0 WHERE event_quota = 1000000;
