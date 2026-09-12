-- backward-compatible: yes (новая таблица, старый бинарь её не читает)
--
-- Внешнего ключа на projects здесь нет намеренно: заявка обязана пережить строку projects —
-- CASCADE снёс бы её вместе с проектом, ради очистки которого она заводится, RESTRICT
-- запретил бы само удаление.
CREATE TABLE IF NOT EXISTS project_purge_queue (
    project_id      bigint PRIMARY KEY,
    enqueued_at     timestamptz NOT NULL DEFAULT now(),
    attempts        integer     NOT NULL DEFAULT 0,
    last_attempt_at timestamptz,
    last_error      text
);

-- Обычный CREATE INDEX, не CONCURRENTLY: таблица заводится пустой этой же миграцией.
CREATE INDEX IF NOT EXISTS project_purge_queue_enqueued_at_idx
    ON project_purge_queue (enqueued_at);
