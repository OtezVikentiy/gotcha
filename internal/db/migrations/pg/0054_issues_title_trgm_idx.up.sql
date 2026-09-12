-- backward-compatible: yes (новый индекс)
--
-- gin_trgm_ops (pg_trgm, 0053) обслуживает ILIKE по подстроке через триграммы —
-- обычный btree годен только на равенство и префикс. CONCURRENTLY/один индекс
-- на файл/ловушка невалидного индекса — тот же приём, что в 0031.
CREATE INDEX CONCURRENTLY IF NOT EXISTS issues_title_trgm_idx
    ON issues USING gin (title gin_trgm_ops);
