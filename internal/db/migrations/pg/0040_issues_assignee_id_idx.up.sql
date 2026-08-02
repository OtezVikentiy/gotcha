-- backward-compatible: yes (новый индекс)
--
-- Находка №45, тот же дефект, что у 0037 (см. его докблок и
-- task-2-report.md) — сама колонка, названная примером прямо в брифе.
-- assignee_id допускает NULL (REFERENCES users(id) ON DELETE SET NULL —
-- проблема может быть не назначена никому) — индекс частичный, только по
-- назначенным строкам: меньше полного, а для проверки каскада при удалении
-- пользователя (SET NULL всем его назначениям) этого достаточно.
CREATE INDEX CONCURRENTLY IF NOT EXISTS issues_assignee_id_idx
    ON issues (assignee_id) WHERE assignee_id IS NOT NULL;
