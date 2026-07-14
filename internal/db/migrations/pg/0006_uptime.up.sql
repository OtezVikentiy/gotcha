CREATE TABLE monitors (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('http','tcp','dns','heartbeat')),
    enabled boolean NOT NULL DEFAULT true,
    interval_seconds int NOT NULL CHECK (interval_seconds >= 30),
    timeout_seconds int NOT NULL DEFAULT 10 CHECK (timeout_seconds BETWEEN 1 AND 120),
    config jsonb NOT NULL DEFAULT '{}'::jsonb,
    fail_threshold int NOT NULL DEFAULT 3 CHECK (fail_threshold >= 1),
    recovery_threshold int NOT NULL DEFAULT 2 CHECK (recovery_threshold >= 1),
    consensus text NOT NULL DEFAULT 'majority' CHECK (consensus IN ('any','majority','all')),
    remind_every_minutes int NOT NULL DEFAULT 0 CHECK (remind_every_minutes >= 0),
    ssl_alert_days int NOT NULL DEFAULT 14 CHECK (ssl_alert_days >= 0),
    ssl_expires_at timestamptz,
    ssl_alerted_days int[] NOT NULL DEFAULT '{}',
    heartbeat_token text UNIQUE,
    last_beat_at timestamptz,
    last_scheduled_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX monitors_project_idx ON monitors (project_id);

CREATE TABLE monitor_regions (
    monitor_id bigint NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    region text NOT NULL,
    PRIMARY KEY (monitor_id, region)
);

CREATE TABLE monitor_channels (
    monitor_id bigint NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    channel_id bigint NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, channel_id)
);

CREATE TABLE monitor_state (
    monitor_id bigint NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    region text NOT NULL,
    status text NOT NULL DEFAULT 'unknown' CHECK (status IN ('up','down','unknown')),
    consecutive_fails int NOT NULL DEFAULT 0,
    consecutive_oks int NOT NULL DEFAULT 0,
    last_checked_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    PRIMARY KEY (monitor_id, region)
);

CREATE TABLE probes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    org_id bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    region text NOT NULL,
    name text NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    last_seen_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX probes_org_region_idx ON probes (org_id, region);

CREATE TABLE check_queue (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    monitor_id bigint NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    region text NOT NULL,
    due_at timestamptz NOT NULL DEFAULT now(),
    leased_by bigint REFERENCES probes(id) ON DELETE SET NULL,
    lease_until timestamptz,
    UNIQUE (monitor_id, region)
);
CREATE INDEX check_queue_region_due_idx ON check_queue (region, due_at);

CREATE TABLE incidents (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    monitor_id bigint NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    started_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    cause text NOT NULL DEFAULT '',
    regions text[] NOT NULL DEFAULT '{}',
    in_maintenance boolean NOT NULL DEFAULT false,
    notified_open boolean NOT NULL DEFAULT false,
    notified_close boolean NOT NULL DEFAULT false,
    last_reminded_at timestamptz
);
CREATE INDEX incidents_monitor_started_idx ON incidents (monitor_id, started_at DESC);
-- на монитор — не более одного открытого инцидента
CREATE UNIQUE INDEX incidents_one_open_idx ON incidents (monitor_id) WHERE resolved_at IS NULL;

CREATE TABLE maintenance_windows (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name text NOT NULL,
    weekly boolean NOT NULL DEFAULT false,
    starts_at timestamptz,          -- разовое окно
    ends_at timestamptz,
    weekday int,                    -- 0..6 (вс..сб), для weekly
    start_time time,                -- для weekly
    end_time time,
    timezone text NOT NULL DEFAULT 'UTC',
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK ( (weekly AND weekday IS NOT NULL AND start_time IS NOT NULL AND end_time IS NOT NULL)
         OR (NOT weekly AND starts_at IS NOT NULL AND ends_at IS NOT NULL AND ends_at > starts_at) )
);
CREATE INDEX maintenance_windows_project_idx ON maintenance_windows (project_id);

CREATE TABLE status_pages (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    slug text NOT NULL UNIQUE,
    title text NOT NULL,
    description text NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE status_page_monitors (
    status_page_id bigint NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    monitor_id bigint NOT NULL REFERENCES monitors(id) ON DELETE CASCADE,
    display_name text NOT NULL,
    position int NOT NULL DEFAULT 0,
    PRIMARY KEY (status_page_id, monitor_id)
);
