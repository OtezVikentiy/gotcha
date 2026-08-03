-- backward-compatible: yes (новая таблица, старый бинарь её не читает)
--
-- Находка №7 (ARCH-4). Удаление проекта чистило ClickHouse синхронно в
-- HTTP-запросе: восемь мутаций подряд с mutations_sync = 2 и снятым потолком
-- времени. Удаление организации с двадцатью проектами — до ста шестидесяти
-- таких мутаций в одном запросе; WriteTimeout обрывал соединение на третьем
-- проекте, и телеметрия остальных оставалась в ClickHouse навсегда, уже
-- неадресуемая: идентификатора удалённого проекта нет ни в PostgreSQL, ни
-- где-либо ещё.
--
-- ВНЕШНЕГО КЛЮЧА НА projects ЗДЕСЬ НЕТ НАМЕРЕННО. Ссылка сделала бы заявку
-- бессмысленной: ON DELETE CASCADE снёс бы её вместе с проектом, ради
-- очистки которого она и заводится, а RESTRICT запретил бы само удаление.
-- Заявка обязана пережить строку projects — это её единственная работа.
-- (Сторож «внешний ключ без индекса» из подпроекта E1 к этой таблице
-- поэтому неприменим: ключа нет.)
CREATE TABLE IF NOT EXISTS project_purge_queue (
    project_id      bigint PRIMARY KEY,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    last_attempt_at timestamptz,
    last_error      text
);

-- Индекс под выборку следующей заявки (ORDER BY enqueued_at LIMIT 1).
-- Создаётся обычным способом, не CONCURRENTLY: таблица заводится пустой этой
-- же миграцией, блокировать нечего.
CREATE INDEX IF NOT EXISTS project_purge_queue_enqueued_at_idx
    ON project_purge_queue (enqueued_at);
