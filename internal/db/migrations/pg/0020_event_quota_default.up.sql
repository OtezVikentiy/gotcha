-- RA-6: миграция 0018 (PROD-B2) сбросила legacy-дефолты transaction/metric/profile
-- квот на 0 (OSS-безлимит), но event_quota пропустила. Здесь доводим до конца:
-- меняем DEFAULT колонки на 0 и сбрасываем строки, всё ещё держащие legacy-дефолт
-- 1000000. Явно заданные оператором иные значения не трогаются (WHERE = старый дефолт).
ALTER TABLE organizations ALTER COLUMN event_quota SET DEFAULT 0;
UPDATE organizations SET event_quota = 0 WHERE event_quota = 1000000;
