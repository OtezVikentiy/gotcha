-- Троттлинг алертов о производительности: у них нет ни alert_rules, ни
-- throttle_minutes (в отличие от алертов об ошибках), а КАЖДАЯ новая пара
-- (project_id, fingerprint) шлёт задачу в outbox на каждый канал. Один проект,
-- у которого детекция нашла проблему на двух сотнях эндпойнтов, залил бы
-- дежурному две сотни сообщений за минуты. Окно — «прыгающее» (tumbling):
-- window_start + счётчик, одна строка на проект (см. trace.OutboxNotifier).
CREATE TABLE perf_alert_throttle (
    project_id   bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL DEFAULT now(),
    sent         int NOT NULL DEFAULT 0
);

-- sample_at — когда в последний раз обновлялись evidence и sample_trace_id.
-- Без этой колонки повторное обнаружение переписывало бы jsonb-evidence на
-- КАЖДОЙ семплированной транзакции (лишний WAL и TOAST на горячей строке);
-- с ней пример освежается не чаще раза в час (см. trace.IssueService.Record).
ALTER TABLE perf_issues ADD COLUMN sample_at timestamptz NOT NULL DEFAULT now();
