-- backward-compatible: yes (аддитивно; маркер про forward-compat up, не про деструктивность down)
--
-- Те же колонки эскалации, что 0077 дала пяти другим источникам инцидентов, здесь — incidents
-- (uptime). severity DEFAULT 'critical', как у host_incidents/slo_incidents.
ALTER TABLE incidents ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE incidents ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE incidents ADD COLUMN severity text NOT NULL DEFAULT 'critical' CHECK (severity IN ('critical','warning'));
ALTER TABLE incidents ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE incidents ADD COLUMN last_escalated_at timestamptz;

-- Покрывает FK acknowledged_by, симметрично 0077.
CREATE INDEX incidents_acknowledged_by_idx ON incidents (acknowledged_by) WHERE acknowledged_by IS NOT NULL;

-- То, что планировщик перебирает на каждом тике; incidents не несёт project_id напрямую
-- (только monitor_id -> monitors.project_id), поэтому индекс без project_id-компоненты.
CREATE INDEX incidents_esc_pending_idx ON incidents (id) WHERE resolved_at IS NULL AND acknowledged_at IS NULL;

-- notified_open=false раньше означало и «не уведомляли осознанно», и «пытались, канал упал» —
-- notify_open_failed отделяет эти случаи и разрешает быстрый ретрай. notify_open_attempts —
-- граница попыток (maxNotifyOpenAttempts), не даёт мёртвому каналу пейджиться бесконечно.
ALTER TABLE incidents ADD COLUMN notify_open_failed boolean NOT NULL DEFAULT false;
ALTER TABLE incidents ADD COLUMN notify_open_attempts int NOT NULL DEFAULT 0;

-- Уже отправившие open-уведомление считаем состоявшимся шагом 0, симметрично backfill 0077.
UPDATE incidents SET escalation_level = 1 WHERE resolved_at IS NULL AND notified_open = true;

-- Тот же приём, что 0077: JOIN на ac.enabled, не Deliverable() — недоставляемые каналы
-- отфильтровывает отправка, не эта миграция.
INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'uptime', i.id, ac.id, 0, now()
    FROM incidents i
    JOIN monitors m ON m.id = i.monitor_id
    JOIN alert_channels ac ON ac.project_id = m.project_id AND ac.enabled
    WHERE i.resolved_at IS NULL AND i.notified_open = true;
