CREATE TABLE issue_environments (
    issue_id    bigint NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    project_id  bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    environment text NOT NULL,
    PRIMARY KEY (issue_id, environment)
);
CREATE INDEX issue_environments_project_env_idx ON issue_environments (project_id, environment);
