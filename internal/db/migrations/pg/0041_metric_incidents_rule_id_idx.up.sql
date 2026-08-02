-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). rule_id NOT NULL — индекс полный.
--
-- В таблице уже есть metric_incidents_one_open_idx, ведущий с rule_id, но он
-- партиционный (WHERE status = 'open') — для проверки каскада при удалении
-- правила это не покрытие: закрытые инциденты (status <> 'open') под этим
-- условием не лежат, и полный скан для них никуда не девается. Каталожный
-- запрос задачи (task-2-report.md) это учитывает и намеренно не засчитывает
-- партиционные индексы на постороннее условие как покрытие.
CREATE INDEX CONCURRENTLY IF NOT EXISTS metric_incidents_rule_id_idx
    ON metric_incidents (rule_id);
