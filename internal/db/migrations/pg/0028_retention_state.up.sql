-- backward-compatible: yes (новая таблица)
--
-- TTL ClickHouse — свойство инсталляции, но задавался окружением каждой реплики: разные
-- значения перекидывали TTL туда-обратно (ALTER TABLE MODIFY TTL — пересчёт по всем кускам).
CREATE TABLE retention_state (
    key        text PRIMARY KEY,
    days       int  NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT now()
);
