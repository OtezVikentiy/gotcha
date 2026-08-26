-- backward-compatible: yes (аддитивно — новая таблица и её индексы)
-- Заявки на фоновую выгрузку ошибок и событий. Таблица работает как очередь:
-- воркер берёт заявку через FOR UPDATE SKIP LOCKED и держит лизу в claimed_at,
-- поэтому падение инстанса не оставляет заявку висеть навсегда.
CREATE TABLE export_jobs (
    id             bigserial PRIMARY KEY,
    project_id     bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_by     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind           text NOT NULL CHECK (kind IN ('issues','events')),
    format         text NOT NULL CHECK (format IN ('csv','json','ndjson')),
    -- Без FK на issues: группы чистятся ретеншном, а история заявки должна
    -- пережить группу — иначе каскад молча снесёт запись о выгрузке.
    scope_issue_id bigint,
    params         jsonb NOT NULL DEFAULT '{}'::jsonb,
    include_pii    boolean NOT NULL DEFAULT false,
    status         text NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','running','done','failed','expired')),
    attempts       int NOT NULL DEFAULT 0,
    last_error     text NOT NULL DEFAULT '',
    rows_written   bigint NOT NULL DEFAULT 0,
    bytes          bigint NOT NULL DEFAULT 0,
    truncated      boolean NOT NULL DEFAULT false,
    file_ext       text NOT NULL DEFAULT '',
    claimed_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    expires_at     timestamptz
);

-- Клейм ходит только по незавершённым заявкам, частичный индекс держит его
-- маленьким независимо от истории выгрузок.
CREATE INDEX export_jobs_claim_idx  ON export_jobs (created_at)
    WHERE status IN ('queued','running');
CREATE INDEX export_jobs_list_idx   ON export_jobs (project_id, created_at DESC);
CREATE INDEX export_jobs_expire_idx ON export_jobs (expires_at) WHERE status = 'done';
