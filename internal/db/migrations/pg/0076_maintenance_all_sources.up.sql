-- backward-compatible: no (DROP CONSTRAINT maintenance_windows_check)
-- B3: подавление уведомлений окнами обслуживания для всех источников. Флаг ставится
-- при открытии инцидента в окне; гейтит open+close notify (зеркало uptime.incidents).
-- ADD COLUMN x5 сами по себе обратно-совместимы, но страж (internal/guards/
-- migrations_test.go) трактует ЛЮБОЙ DROP CONSTRAINT в миграции как разрушительный
-- независимо от направления замены — тот же урок, что и pg/0029 (маркер "yes" там
-- оказался верным по случайности, не по проверке). Формально замена здесь СМЯГЧАЕТ
-- CHECK (старые строки/старый бинарь продолжают писать валидные данные), но старый
-- бинарь, читающий ends_at как гарантированно NOT NULL, увидит NULL у бессрочных
-- окон, созданных новым бинарём в момент смешанного релиза — так что fail-closed
-- здесь корректен: старый бинарь должен отказаться стартовать на схеме версии ≥76,
-- а не молча деградировать.
ALTER TABLE host_incidents      ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE metric_incidents    ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE perf_regressions    ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE profile_regressions ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE slo_incidents       ADD COLUMN in_maintenance BOOLEAN NOT NULL DEFAULT false;

-- «Бессрочно»: разовое окно с ends_at NULL. Сохраняем guard ends_at>starts_at для конечных.
-- Старый CHECK (0006_uptime.up.sql) объявлен inline без имени — PG дал автоимя
-- maintenance_windows_check (единственный unnamed CHECK на этой таблице).
ALTER TABLE maintenance_windows DROP CONSTRAINT maintenance_windows_check;
ALTER TABLE maintenance_windows ADD CONSTRAINT maintenance_windows_shape CHECK (
  (weekly AND weekday IS NOT NULL AND start_time IS NOT NULL AND end_time IS NOT NULL)
  OR
  (NOT weekly AND starts_at IS NOT NULL AND (ends_at IS NULL OR ends_at > starts_at))
);
