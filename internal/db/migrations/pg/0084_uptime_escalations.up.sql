-- backward-compatible: yes (ADD COLUMN с дефолтами + новая FK-колонка с индексом,
-- аддитивно, старый бинарь переживёт схему-вперёд — как 0077/0079). Маркер про
-- forward-compat up, не про деструктивность down.
--
-- W2-C, находки 1 и 2 аудита 2026-08-27 (кластер 1, DEDUP-P1.md).
--
-- Находка 2: uptime-инциденты (таблица incidents) — единственный из 6 источников,
-- не заведённый в контур эскалаций (B4, миграция 0077 дала acknowledged_at/
-- acknowledged_by/severity/escalation_level/last_escalated_at пяти остальным
-- таблицам, incidents пропущена). Колонки — те же, семантика та же:
-- severity DEFAULT 'critical' (падение сайта — инфраструктурный сбой, тот же
-- default, что у host_incidents/slo_incidents).
ALTER TABLE incidents ADD COLUMN acknowledged_at timestamptz;
ALTER TABLE incidents ADD COLUMN acknowledged_by bigint REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE incidents ADD COLUMN severity text NOT NULL DEFAULT 'critical' CHECK (severity IN ('critical','warning'));
ALTER TABLE incidents ADD COLUMN escalation_level int NOT NULL DEFAULT 0;
ALTER TABLE incidents ADD COLUMN last_escalated_at timestamptz;

-- Покрытие FK acknowledged_by (internal/guards/fkindex_test.go), симметрично 0077.
CREATE INDEX incidents_acknowledged_by_idx ON incidents (acknowledged_by) WHERE acknowledged_by IS NOT NULL;

-- Открытые неподтверждённые инциденты — то, что планировщик эскалации
-- перебирает на каждом тике (Source.OpenUnacked); incidents не несёт
-- project_id напрямую (только monitor_id -> monitors.project_id), поэтому
-- индекс без project_id-компоненты, симметрично 0077 esc_pending_idx по духу,
-- не по колонкам.
CREATE INDEX incidents_esc_pending_idx ON incidents (id) WHERE resolved_at IS NULL AND acknowledged_at IS NULL;

-- Находка 1: провал доставки "down" сейчас глушит и "down", и "up" навсегда —
-- notified_open остаётся false и для "сознательно не уведомляли" (окно
-- обслуживания, B5-подавление), и для "пытались, канал упал". Явный признак
-- неудачной попытки отделяет эти два случая: только он разрешает быстрый
-- ретрай на следующем тике (без ожидания SettleGrace и без завязки на B5-
-- dep-сервис), не трогая семантику notified_open/suppressed_by_dep/
-- in_maintenance. notify_open_attempts — граница числа попыток (см.
-- internal/uptime/detector.go, maxNotifyOpenAttempts) — не даёт мёртвому
-- каналу пейджиться бесконечно.
ALTER TABLE incidents ADD COLUMN notify_open_failed boolean NOT NULL DEFAULT false;
ALTER TABLE incidents ADD COLUMN notify_open_attempts int NOT NULL DEFAULT 0;

-- Существующие открытые+отнотифаенные uptime-инциденты уже отправили open-
-- уведомление до появления эскалаций — считаем это состоявшимся шагом 0,
-- симметрично backfill 0077 для остальных пяти таблиц.
UPDATE incidents SET escalation_level = 1 WHERE resolved_at IS NULL AND notified_open = true;

-- Синтетический step0-лог: по одной строке на (инцидент, включённый канал
-- проекта монитора) — тот же приём, что 0077, JOIN на ac.enabled, не
-- Deliverable() (недоставляемые каналы отфильтровывает отправка, не миграция).
INSERT INTO incident_escalations (incident_source, incident_id, channel_id, step, sent_at)
    SELECT 'uptime', i.id, ac.id, 0, now()
    FROM incidents i
    JOIN monitors m ON m.id = i.monitor_id
    JOIN alert_channels ac ON ac.project_id = m.project_id AND ac.enabled
    WHERE i.resolved_at IS NULL AND i.notified_open = true;
