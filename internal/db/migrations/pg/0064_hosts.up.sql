-- backward-compatible: yes
CREATE TABLE hosts (
    id         bigserial PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       text   NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now(),
    last_seen  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);
