-- backward-compatible: yes (новые колонки со значением по умолчанию)
--
-- Блокировка строки org_usage остаётся — сокращается число обращений, на время которых она
-- удерживается. PG17 RETURNING не отдаёт старую строку (RETURNING OLD — только в 18), поэтому
-- предобраз сохраняется тем же оператором; с переходом на PG18 эти колонки уходят миграцией.
ALTER TABLE org_usage
    ADD COLUMN IF NOT EXISTS events_count_before       bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transactions_count_before bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS metrics_count_before      bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS profiles_count_before     bigint NOT NULL DEFAULT 0;
