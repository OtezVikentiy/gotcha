-- Этап 6 (метрики): квота метрик (per-request счётчик, как транзакции) и
-- пороговые алерты на метрики (модель регрессий этапа 4: правило + инцидент
-- open/close, один открытый инцидент на правило).
ALTER TABLE organizations ADD COLUMN metric_quota bigint NOT NULL DEFAULT 1000000;
ALTER TABLE org_usage ADD COLUMN metrics_count bigint NOT NULL DEFAULT 0;

CREATE TABLE metric_alert_rules (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    metric_name text NOT NULL,
    aggregation text NOT NULL CHECK (aggregation IN ('avg','max','min','sum','p50','p95','p99')),
    comparator  text NOT NULL CHECK (comparator IN ('gt','lt')),
    threshold   double precision NOT NULL,
    window_seconds int NOT NULL DEFAULT 300 CHECK (window_seconds > 0),
    environment text,
    label_key text,
    label_value text,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX metric_alert_rules_project_idx ON metric_alert_rules (project_id);

CREATE TABLE metric_incidents (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rule_id bigint NOT NULL REFERENCES metric_alert_rules(id) ON DELETE CASCADE,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
    peak_value double precision NOT NULL,
    current_value double precision NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    notified_open boolean NOT NULL DEFAULT false,
    notified_close boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX metric_incidents_one_open_idx ON metric_incidents (rule_id) WHERE status = 'open';
CREATE INDEX metric_incidents_project_started_idx ON metric_incidents (project_id, started_at DESC);
