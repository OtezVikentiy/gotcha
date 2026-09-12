-- backward-compatible: yes (аддитивно; маркер про forward-compat up, не про деструктивность down)
-- incident_source: host_incidents->'host', metric_incidents->'metric', perf_regressions->'trace',
-- profile_regressions->'profile', slo_incidents->'slo'.

-- severity DEFAULT разный по таблице: host/slo — 'critical', metric/trace(perf)/profile — 'warning'.
ALTER TABLE host_incidents      ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE host_incidents      ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE host_incidents      ADD COLUMN severity text NOT NULL DEFAULT 'critical' CHECK (severity IN ('critical','warning'));
ALTER TABLE host_incidents      ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE host_incidents      ADD COLUMN last_escalated_at timestamptz;

ALTER TABLE metric_incidents    ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE metric_incidents    ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE metric_incidents    ADD COLUMN severity text NOT NULL DEFAULT 'warning' CHECK (severity IN ('critical','warning'));
ALTER TABLE metric_incidents    ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE metric_incidents    ADD COLUMN last_escalated_at timestamptz;

ALTER TABLE perf_regressions    ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE perf_regressions    ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE perf_regressions    ADD COLUMN severity text NOT NULL DEFAULT 'warning' CHECK (severity IN ('critical','warning'));
ALTER TABLE perf_regressions    ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE perf_regressions    ADD COLUMN last_escalated_at timestamptz;

ALTER TABLE profile_regressions ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE profile_regressions ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE profile_regressions ADD COLUMN severity text NOT NULL DEFAULT 'warning' CHECK (severity IN ('critical','warning'));
ALTER TABLE profile_regressions ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE profile_regressions ADD COLUMN last_escalated_at timestamptz;

ALTER TABLE slo_incidents       ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE slo_incidents       ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE slo_incidents       ADD COLUMN severity text NOT NULL DEFAULT 'critical' CHECK (severity IN ('critical','warning'));
ALTER TABLE slo_incidents       ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE slo_incidents       ADD COLUMN last_escalated_at timestamptz;

-- NULL — использовать table-DEFAULT источника выше.
ALTER TABLE metric_alert_rules ADD COLUMN severity text
    CHECK (severity IS NULL OR severity IN ('critical','warning'));

CREATE TABLE escalation_steps (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    severity text NOT NULL CHECK (severity IN ('critical','warning')),
    step_no int NOT NULL,
    delay_minutes int NOT NULL CHECK (delay_minutes >= 0),
    UNIQUE (project_id, severity, step_no)
);

CREATE TABLE escalation_step_channels (
    step_id bigint NOT NULL REFERENCES escalation_steps(id) ON DELETE CASCADE,
    channel_id bigint NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (step_id, channel_id)
);
-- PK покрывает step_id как ведущую, не channel_id — гейт fkindex_test.go требует
-- отдельный индекс, начинающийся со ссылающейся колонки.
CREATE INDEX escalation_step_channels_channel_id_idx ON escalation_step_channels (channel_id);

-- Без FK: channel_id — канал мог быть удалён после отправки, лог остаётся;
-- incident_id — 5 разных исходных таблиц, различаются incident_source.
CREATE TABLE incident_escalations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    incident_source text NOT NULL,
    incident_id bigint NOT NULL,
    channel_id bigint NOT NULL,
    step int NOT NULL,
    sent_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX incident_escalations_incident_idx ON incident_escalations (incident_source, incident_id);

-- Только открытые и ещё не подтверждённые — то, что планировщик перебирает на каждом тике.
CREATE INDEX host_incidents_esc_pending_idx      ON host_incidents      (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX metric_incidents_esc_pending_idx    ON metric_incidents    (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX perf_regressions_esc_pending_idx    ON perf_regressions    (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX profile_regressions_esc_pending_idx ON profile_regressions (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX slo_incidents_esc_pending_idx       ON slo_incidents       (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;

-- Покрывает FK acknowledged_by, частичный — как issues_assignee_id_idx (0040).
CREATE INDEX host_incidents_acknowledged_by_idx      ON host_incidents      (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX metric_incidents_acknowledged_by_idx    ON metric_incidents    (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX perf_regressions_acknowledged_by_idx    ON perf_regressions    (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX profile_regressions_acknowledged_by_idx ON profile_regressions (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX slo_incidents_acknowledged_by_idx       ON slo_incidents       (acknowledged_by) WHERE acknowledged_by IS NOT NULL;

-- Уже отправившие open-уведомление считаем состоявшимся шагом 0 — иначе планировщик
-- зашлёт step0 повторно, а recovery не найдёт лог для закрытия.
UPDATE host_incidents      SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE metric_incidents    SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE perf_regressions    SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE profile_regressions SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE slo_incidents       SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;

-- JOIN на ac.enabled, не Deliverable() — лог может содержать недоставляемые каналы
-- (например webhook без секрета), их фильтрует отправка, не эта миграция.
INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'host', i.id, ac.id, 0, now()
    FROM host_incidents i
    JOIN alert_channels ac ON ac.project_id = i.project_id AND ac.enabled
    WHERE i.status = 'open' AND i.notified_open = true;

INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'metric', i.id, ac.id, 0, now()
    FROM metric_incidents i
    JOIN alert_channels ac ON ac.project_id = i.project_id AND ac.enabled
    WHERE i.status = 'open' AND i.notified_open = true;

INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'trace', i.id, ac.id, 0, now()
    FROM perf_regressions i
    JOIN alert_channels ac ON ac.project_id = i.project_id AND ac.enabled
    WHERE i.status = 'open' AND i.notified_open = true;

INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'profile', i.id, ac.id, 0, now()
    FROM profile_regressions i
    JOIN alert_channels ac ON ac.project_id = i.project_id AND ac.enabled
    WHERE i.status = 'open' AND i.notified_open = true;

INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'slo', i.id, ac.id, 0, now()
    FROM slo_incidents i
    JOIN alert_channels ac ON ac.project_id = i.project_id AND ac.enabled
    WHERE i.status = 'open' AND i.notified_open = true;
