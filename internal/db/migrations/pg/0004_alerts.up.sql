CREATE TABLE alert_rules (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('new_issue','regression','spike')),
    enabled boolean NOT NULL DEFAULT true,
    threshold int NOT NULL DEFAULT 0,        -- для spike: N событий
    window_minutes int NOT NULL DEFAULT 0,   -- для spike: за M минут
    throttle_minutes int NOT NULL DEFAULT 30,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, kind)
);
CREATE TABLE alert_channels (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('email','webhook','telegram')),
    enabled boolean NOT NULL DEFAULT true,
    target text NOT NULL,      -- email: адрес; webhook: URL; telegram: chat_id
    secret text NOT NULL DEFAULT '', -- webhook: HMAC secret; telegram: bot token
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE notification_outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    channel_id bigint NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    payload jsonb NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed')),
    attempts int NOT NULL DEFAULT 0,
    last_error text NOT NULL DEFAULT '',
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    sent_at timestamptz
);
CREATE INDEX outbox_due_idx ON notification_outbox (status, next_retry_at);
CREATE TABLE alert_throttle (
    issue_id bigint NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    rule_id bigint NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    last_sent_at timestamptz NOT NULL,
    PRIMARY KEY (issue_id, rule_id)
);
CREATE TABLE org_usage (
    org_id bigint NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    period_month date NOT NULL,
    events_count bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (org_id, period_month)
);
