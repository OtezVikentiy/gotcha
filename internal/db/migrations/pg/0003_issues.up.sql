CREATE TABLE issues (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id  bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    fingerprint text NOT NULL,
    title       text NOT NULL,
    culprit     text NOT NULL DEFAULT '',
    level       text NOT NULL DEFAULT 'error',
    status      text NOT NULL DEFAULT 'unresolved'
                CHECK (status IN ('unresolved','resolved','ignored')),
    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz NOT NULL DEFAULT now(),
    times_seen  bigint NOT NULL DEFAULT 1,
    assignee_id bigint REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (project_id, fingerprint)
);
CREATE INDEX issues_project_last_seen_idx ON issues (project_id, last_seen DESC);
