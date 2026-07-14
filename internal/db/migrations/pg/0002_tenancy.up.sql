CREATE TABLE users (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    email         citext NOT NULL UNIQUE,
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash bytea PRIMARY KEY,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE organizations (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug        text NOT NULL UNIQUE,
    name        text NOT NULL,
    event_quota bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE org_members (
    org_id     bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner','admin','member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id)
);

CREATE TABLE org_invites (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id      bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    email       citext NOT NULL,
    role        text NOT NULL CHECK (role IN ('admin','member')),
    token_hash  bytea NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug       text NOT NULL,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE TABLE team_members (
    team_id bigint NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (team_id, user_id)
);

CREATE TABLE projects (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id     bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    slug       text NOT NULL,
    name       text NOT NULL,
    platform   text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, slug)
);

CREATE TABLE project_teams (
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_id    bigint NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    PRIMARY KEY (project_id, team_id)
);

CREATE TABLE project_keys (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    public_key text NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
