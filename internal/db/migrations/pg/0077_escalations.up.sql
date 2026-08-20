-- backward-compatible: no (down деструктивен — DROP TABLE/DROP COLUMN; консервативно
-- запрещаем откат релиза сквозь эту версию, хотя сам up только добавляет)
-- B4: эскалации. Источник инцидентов зафиксирован строкой incident_source, консистентно
-- с планировщиком (T4) и recovery (T6): host_incidents→'host', metric_incidents→'metric',
-- perf_regressions→'trace', profile_regressions→'profile', slo_incidents→'slo'.

-- Подтверждение (ack), приоритет и текущий шаг эскалации — на каждой из 5 однородных
-- инцидент-таблиц (все: status CHECK IN ('open','resolved'), notified_open, project_id).
-- severity DEFAULT разный по таблице: host/slo — 'critical' (инфраструктура и SLO-бюджет
-- по умолчанию громче), metric/trace(perf)/profile — 'warning'.
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

-- Override severity на конкретное metric-правило (NULL = использовать table-DEFAULT
-- источника выше). Проставляется в T5 (UI/API правил).
ALTER TABLE metric_alert_rules ADD COLUMN severity text
    CHECK (severity IS NULL OR severity IN ('critical','warning'));

-- Политика эскалации: набор шагов на (проект, severity), каждый шаг — задержка от
-- открытия инцидента и набор каналов, в которые уходит уведомление на этом шаге.
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
-- PRIMARY KEY (step_id, channel_id) покрывает step_id как ведущую колонку, но не
-- channel_id — гейт internal/guards/fkindex_test.go требует индекс, начинающийся
-- именно со ссылающейся колонки, для каскадного удаления канала.
CREATE INDEX escalation_step_channels_channel_id_idx ON escalation_step_channels (channel_id);

-- Лог отправленных эскалаций — исторический: channel_id БЕЗ FK (канал может быть
-- удалён после отправки, лог остаётся; фильтр по доставляемости — на отправке, не
-- здесь), incident_id БЕЗ FK (5 разных исходных таблиц, различаются incident_source).
CREATE TABLE incident_escalations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    incident_source text NOT NULL,
    incident_id bigint NOT NULL,
    channel_id bigint NOT NULL,
    step int NOT NULL,
    sent_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX incident_escalations_incident_idx ON incident_escalations (incident_source, incident_id);

-- Partial-индексы планировщика: только открытые и ещё не подтверждённые инциденты —
-- это ровно то, что планировщик перебирает на каждом тике.
CREATE INDEX host_incidents_esc_pending_idx      ON host_incidents      (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX metric_incidents_esc_pending_idx    ON metric_incidents    (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX perf_regressions_esc_pending_idx    ON perf_regressions    (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX profile_regressions_esc_pending_idx ON profile_regressions (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;
CREATE INDEX slo_incidents_esc_pending_idx       ON slo_incidents       (project_id) WHERE status = 'open' AND acknowledged_at IS NULL;

-- Покрытие FK acknowledged_by (internal/guards/fkindex_test.go): частичный индекс —
-- только по фактически подтверждённым, симметрично issues_assignee_id_idx (0040).
-- Колонка только что добавлена (все строки NULL), матчит 0 строк на момент создания.
CREATE INDEX host_incidents_acknowledged_by_idx      ON host_incidents      (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX metric_incidents_acknowledged_by_idx    ON metric_incidents    (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX perf_regressions_acknowledged_by_idx    ON perf_regressions    (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX profile_regressions_acknowledged_by_idx ON profile_regressions (acknowledged_by) WHERE acknowledged_by IS NOT NULL;
CREATE INDEX slo_incidents_acknowledged_by_idx       ON slo_incidents       (acknowledged_by) WHERE acknowledged_by IS NOT NULL;

-- Существующие открытые+отнотифаенные инциденты уже отправили open-уведомление до
-- появления эскалаций — считаем это состоявшимся шагом 0. Без этого планировщик на
-- первом тике зашлёт step0 повторно (escalation_level=0 читается как «шаг 0 ещё не
-- отправлен»), а recovery (T6) не найдёт лог, в который слать «тем же» при закрытии.
UPDATE host_incidents      SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE metric_incidents    SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE perf_regressions    SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE profile_regressions SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;
UPDATE slo_incidents       SET escalation_level = 1 WHERE status = 'open' AND notified_open = true;

-- Синтетический step0-лог: по одной строке на (инцидент, включённый канал проекта).
-- JOIN на ac.enabled, не Deliverable() — лог может содержать недоставляемые на момент
-- миграции каналы (например webhook без секрета), recovery (T6) фильтрует на отправке.
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
