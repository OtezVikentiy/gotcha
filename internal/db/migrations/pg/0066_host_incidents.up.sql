-- backward-compatible: yes
CREATE TABLE host_incidents (
    id bigserial PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    host_id    bigint NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('disk','memory','load','silent')),
    status text NOT NULL CHECK (status IN ('open','resolved')),
    current_value double precision,
    peak_value    double precision,
    detail text NOT NULL DEFAULT '',            -- напр. худший mountpoint
    started_at  timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    notified_open  boolean NOT NULL DEFAULT false,
    notified_close boolean NOT NULL DEFAULT false
);

-- Частичный уникальный индекс — гонко-безопасность IncidentService.Open: на
-- пару (host_id, kind) может существовать не более одного инцидента со
-- status='open' одновременно; конфликтующая вставка ловится ON CONFLICT
-- DO NOTHING, а победитель дочитывается через OpenFor (см. incident.go,
-- калька metric.IncidentService.Open/OpenFor).
--
-- ОБЫЧНЫЙ CREATE UNIQUE INDEX в файле с CREATE TABLE (прецеденты 0006/0013/
-- 0015), а не отдельной миграцией CONCURRENTLY. Таблица здесь только что
-- создана и пуста — блокировать построением нечего, зато CONCURRENTLY даёт
-- отказ, от которого нет автоматического восстановления: оборванная сборка
-- (OOM, таймаут, kill пода) оставляет INVALID-индекс, повторный прогон с
-- IF NOT EXISTS молча его пропускает, а дальше ON CONFLICT в Open не находит
-- арбитра и падает НАВСЕГДА — инциденты хостов перестают открываться, и
-- единственный след этого в журнале — Warn.
CREATE UNIQUE INDEX host_incidents_one_open_idx
    ON host_incidents (host_id, kind)
    WHERE status = 'open';
