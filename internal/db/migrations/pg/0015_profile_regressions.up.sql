-- Этап 9 (регрессии профилей): рост self-CPU доли функции над скользящей базой
-- моделируется инцидентом open/close — та же механика, что perf_regressions
-- этапа 4. Ключ цели — (project_id, service, profile_type, function).
CREATE TABLE profile_regressions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    service text NOT NULL,
    profile_type text NOT NULL,
    function text NOT NULL,
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
    baseline_share double precision NOT NULL,
    peak_share double precision NOT NULL,
    current_share double precision NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    notified_open boolean NOT NULL DEFAULT false,
    notified_close boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX profile_regressions_one_open_idx
    ON profile_regressions (project_id, service, profile_type, function) WHERE status = 'open';
CREATE INDEX profile_regressions_project_started_idx ON profile_regressions (project_id, started_at DESC);
