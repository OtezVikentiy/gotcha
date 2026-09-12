-- backward-compatible: yes (новая таблица)
-- Кап на создание perf_issues: не шаблонизированные пути дают уникальные фингерпринты —
-- без капа сотни тысяч строк в час. Окно tumbling; suppressed уходит в лог, не молча.
CREATE TABLE perf_issue_throttle (
    project_id   bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL DEFAULT now(),
    created      int NOT NULL DEFAULT 0,
    suppressed   int NOT NULL DEFAULT 0
);
