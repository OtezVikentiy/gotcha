-- backward-compatible: yes (новая таблица и ADD COLUMN с дефолтом)
-- Без троттлинга каждая новая (project_id, fingerprint) шлёт задачу в outbox на канал —
-- сотня находок заливает дежурного сотнями сообщений. Окно — tumbling, одна строка на проект.
CREATE TABLE perf_alert_throttle (
    project_id   bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL DEFAULT now(),
    sent         int NOT NULL DEFAULT 0
);

-- sample_at ограничивает обновление evidence раз в час — иначе каждая семплированная
-- транзакция переписывала бы jsonb на горячей строке (лишний WAL/TOAST).
ALTER TABLE perf_issues ADD COLUMN sample_at timestamptz NOT NULL DEFAULT now();
