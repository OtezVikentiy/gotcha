-- backward-compatible: yes (новые таблицы и индексы)
CREATE TABLE slos (
    id            bigserial PRIMARY KEY,
    project_id    bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          text NOT NULL,
    sli_kind      text NOT NULL CHECK (sli_kind IN ('availability','latency','uptime')),
    target        double precision NOT NULL CHECK (target > 0 AND target < 1),
    window_days   int NOT NULL CHECK (window_days BETWEEN 1 AND 90),
    transaction   text NOT NULL DEFAULT '',
    environment   text NOT NULL DEFAULT '',
    threshold_ms  int NOT NULL DEFAULT 0,
    monitor_id    bigint REFERENCES monitors(id) ON DELETE CASCADE,
    burn_threshold     double precision NOT NULL DEFAULT 14.4,
    burn_long_minutes  int NOT NULL DEFAULT 60,
    burn_short_minutes int NOT NULL DEFAULT 5,
    enabled       boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX slos_project_idx ON slos(project_id);
CREATE INDEX slos_monitor_idx ON slos(monitor_id) WHERE monitor_id IS NOT NULL;

CREATE TABLE slo_incidents (
    id            bigserial PRIMARY KEY,
    slo_id        bigint NOT NULL REFERENCES slos(id) ON DELETE CASCADE,
    project_id    bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
    burn_rate     double precision NOT NULL,
    budget_remaining double precision,
    started_at    timestamptz NOT NULL DEFAULT now(),
    resolved_at   timestamptz,
    notified_open  boolean NOT NULL DEFAULT false,
    notified_close boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX slo_incidents_one_open_idx ON slo_incidents(slo_id) WHERE status = 'open';
CREATE INDEX slo_incidents_slo_idx ON slo_incidents(slo_id);
CREATE INDEX slo_incidents_project_started_idx ON slo_incidents(project_id, started_at DESC);
CREATE INDEX slo_incidents_resolved_idx ON slo_incidents(resolved_at) WHERE status = 'resolved';
