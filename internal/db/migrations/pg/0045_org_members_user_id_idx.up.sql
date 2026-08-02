-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). user_id NOT NULL — индекс полный. Первичный ключ
-- таблицы (org_id, user_id) ведёт с org_id и не покрывает поиск по user_id
-- при удалении пользователя.
CREATE INDEX CONCURRENTLY IF NOT EXISTS org_members_user_id_idx
    ON org_members (user_id);
