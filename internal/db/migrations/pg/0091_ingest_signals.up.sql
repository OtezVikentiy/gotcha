-- backward-compatible: yes (новая таблица)
--
-- Аудит перед 1.0, находки K7-5/K7-6: приём отказывает по ключу и принимает
-- запросы на устаревших путях (см. internal/ingest/deprecated.go) молча для
-- оператора конкретного проекта — self-метрики gotcha_ingest_deprecated_path_total
-- и gotcha_ingest_key_rejections_total процесс-локальны и без метки проекта,
-- а у ключей нет last_used_at. ingest_signals — per-project учёт этих же
-- событий: агрегат по (project_id, kind) с суммой попаданий и временем
-- последнего, пишется не на каждый запрос, а раз в 30с из in-memory
-- аккумулятора (internal/ingestsignal.Recorder) — путь неаутентифицированный,
-- и запись в PG на каждый отказ была бы усилителем нагрузки.
--
-- kind — БЕЗ CHECK: закрытый список живёт в коде (internal/ingestsignal.Kind),
-- добавление нового вида сигнала не должно требовать миграции схемы.
CREATE TABLE ingest_signals (
    project_id   bigint      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind         text        NOT NULL,
    hits         bigint      NOT NULL DEFAULT 0,
    last_seen_at timestamptz NOT NULL,
    PRIMARY KEY (project_id, kind)
);
