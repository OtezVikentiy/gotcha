-- backward-compatible: yes (новая таблица)
-- Пер-проектный потолок уведомлений за окно: alert_throttle ключуется (issue_id, rule_id) и
-- не защищает от потока НОВЫХ issue (уникальный fingerprint на каждое событие).
-- allowed пишется тем же оператором, что и счётчик (см. claimBudget) — иначе гонка между
-- проверкой бюджета и списанием. suppressed сбрасывает отдельная задача-сводка (alert.Digester).
CREATE TABLE alert_project_budget (
    project_id   bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL DEFAULT now(),
    sent         integer NOT NULL DEFAULT 0,
    suppressed   integer NOT NULL DEFAULT 0,
    allowed      boolean NOT NULL DEFAULT true,
    digest_at    timestamptz
);
