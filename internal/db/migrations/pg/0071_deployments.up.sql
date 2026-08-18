-- backward-compatible: yes (новая таблица)
CREATE TABLE deployments (
    id          bigserial PRIMARY KEY,
    project_id  bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version     text   NOT NULL,
    environment text   NOT NULL DEFAULT '',
    deployed_at timestamptz NOT NULL,
    url         text   NOT NULL DEFAULT '',
    changelog   text   NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_deployments_project_time ON deployments (project_id, deployed_at DESC);
