-- backward-compatible: yes (новые колонки со значением по умолчанию)
--
-- Находка №8 (ARCH-5). Списание квоты держало блокировку строки
-- org_usage(org_id, period_month) пять сетевых обращений: BEGIN, INSERT …
-- ON CONFLICT DO NOTHING, SELECT … FOR UPDATE, UPDATE, COMMIT. Через эту
-- строку проходит весь приём организации, поэтому потолок упирался в задержку
-- до базы, а не в её ёмкость.
--
-- Блокировка нужна и остаётся: без неё два конкурентных приёма одной
-- организации разойдутся в счётчике. Убирается другое — число обращений, на
-- время которых она удерживается. Чтобы вернуть, СКОЛЬКО списано, нужно
-- значение счётчика до инкремента, а RETURNING в PostgreSQL 17 отдаёт только
-- новую строку (RETURNING OLD появился в 18). Предобраз сохраняется тем же
-- оператором, в эти колонки, и списанное считается вычитанием.
--
-- СРОК ЖИЗНИ РЕШЕНИЯ ОГРАНИЧЕН. С переходом на PostgreSQL 18 конструкция
-- схлопывается в RETURNING OLD, и эти четыре колонки уходят миграцией.
ALTER TABLE org_usage
    ADD COLUMN IF NOT EXISTS events_count_before       bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS transactions_count_before bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS metrics_count_before      bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS profiles_count_before     bigint NOT NULL DEFAULT 0;
