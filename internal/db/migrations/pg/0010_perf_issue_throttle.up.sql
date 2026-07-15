-- Кап на СОЗДАНИЕ новых проблем производительности. Фингерпринт держится на
-- нормализованном имени транзакции, а нормализация маршрутов — эвристика: у
-- приложения, которое не шаблонизирует пути (ObjectID, slug'и, логины), часть
-- имён неизбежно останется уникальной, и каждая давала бы НОВУЮ строку
-- perf_issues. При 50 rps это сотни тысяч строк в час, а retention-задачи для
-- этой таблицы нет. Рассылку алертов ограничивает perf_alert_throttle, но
-- строки в PG он не отменяет — их и режет этот кап (см. trace.IssueService.Record).
--
-- Окно «прыгающее» (tumbling), одна строка на проект: created — сколько новых
-- проблем создано в окне, suppressed — сколько находок не получили строки
-- (число уходит в лог: молчаливый кап читался бы как «мы нашли всё»).
CREATE TABLE perf_issue_throttle (
    project_id   bigint PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    window_start timestamptz NOT NULL DEFAULT now(),
    created      int NOT NULL DEFAULT 0,
    suppressed   int NOT NULL DEFAULT 0
);
