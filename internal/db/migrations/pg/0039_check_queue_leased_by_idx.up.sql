-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md). leased_by допускает NULL (REFERENCES probes(id) ON
-- DELETE SET NULL — задание не обязано быть арендовано выносной пробой) —
-- индекс частичный, только по строкам с фактической арендой: он меньше
-- полного, а для проверки каскада при удалении пробы этого достаточно,
-- проверка каскада всё равно ищет конкретное non-null значение.
CREATE INDEX CONCURRENTLY IF NOT EXISTS check_queue_leased_by_idx
    ON check_queue (leased_by) WHERE leased_by IS NOT NULL;
