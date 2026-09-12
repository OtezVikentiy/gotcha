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

-- (host_id, kind) WHERE status='open' — не более одного открытого инцидента;
-- гонка ловится ON CONFLICT DO NOTHING, победитель дочитывается через OpenFor.
-- Обычный CREATE UNIQUE INDEX, не CONCURRENTLY: таблица пуста, а невалидный индекс
-- после обрыва построения здесь ломает Open навсегда (ON CONFLICT теряет арбитра).
CREATE UNIQUE INDEX host_incidents_one_open_idx
    ON host_incidents (host_id, kind)
    WHERE status = 'open';
