-- backward-compatible: yes (новая таблица)
--
-- Self-метрики отказов приёма процесс-локальны и без метки проекта — здесь per-project
-- агрегат по (project_id, kind), пишется раз в 30с из аккумулятора, не на каждый запрос
-- (путь неаутентифицированный, иначе усилитель нагрузки).
-- kind — без CHECK: список видов живёт в коде, новый вид не требует миграции схемы.
CREATE TABLE ingest_signals (
    project_id   bigint      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    hits         bigint      NOT NULL DEFAULT 0,
    last_seen_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, kind)
);
