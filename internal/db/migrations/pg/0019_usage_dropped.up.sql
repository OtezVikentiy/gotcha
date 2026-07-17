-- PROD-P1: конец молчаливых потерь. Отдельные счётчики ОТКЛОНЁННЫХ (drop) единиц
-- в той же строке org_usage: сколько событий/транзакций/метрик/профилей приём
-- отбросил за месяц (квота исчерпана и т.п.). Инкрементируются независимо от
-- принятых счётчиков (см. org.IncDropped*), чтобы оператор видел реальные потери.
ALTER TABLE org_usage ADD COLUMN dropped_events bigint NOT NULL DEFAULT 0;
ALTER TABLE org_usage ADD COLUMN dropped_transactions bigint NOT NULL DEFAULT 0;
ALTER TABLE org_usage ADD COLUMN dropped_metrics bigint NOT NULL DEFAULT 0;
ALTER TABLE org_usage ADD COLUMN dropped_profiles bigint NOT NULL DEFAULT 0;
