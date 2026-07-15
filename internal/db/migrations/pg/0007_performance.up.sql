-- kind: 'n_plus_one' | 'slow_db_query' | 'http_flood' (детекторы, спека §5)
CREATE TABLE perf_issues (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id      bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fingerprint     text NOT NULL,
    kind            text NOT NULL,
    title           text NOT NULL,
    culprit         text NOT NULL DEFAULT '',
    status          text NOT NULL DEFAULT 'unresolved'
                    CHECK (status IN ('unresolved','resolved','ignored')),
    count           bigint NOT NULL DEFAULT 0,
    first_seen      timestamptz NOT NULL DEFAULT now(),
    last_seen       timestamptz NOT NULL DEFAULT now(),
    sample_trace_id text NOT NULL DEFAULT '',
    evidence        jsonb NOT NULL DEFAULT '{}'::jsonb,
    UNIQUE (project_id, fingerprint)
);
CREATE INDEX perf_issues_project_last_seen_idx ON perf_issues (project_id, last_seen DESC);

ALTER TABLE projects ADD COLUMN transaction_sample_rate double precision NOT NULL DEFAULT 1.0;
ALTER TABLE projects ADD COLUMN apdex_threshold_ms int NOT NULL DEFAULT 300;
ALTER TABLE projects ADD COLUMN perf_detector_config jsonb NOT NULL DEFAULT '{}'::jsonb;

ALTER TABLE organizations ADD COLUMN transaction_quota bigint NOT NULL DEFAULT 100000;
