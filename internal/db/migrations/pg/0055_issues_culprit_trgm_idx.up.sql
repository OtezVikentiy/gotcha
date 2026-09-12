-- backward-compatible: yes (новый индекс)
--
-- OR по title/culprit (issue/query.go) не даёт планировщику BitmapOr, пока не
-- проиндексированы обе стороны — без этого индекса Seq Scan остаётся, даже с 0054.
CREATE INDEX CONCURRENTLY IF NOT EXISTS issues_culprit_trgm_idx
    ON issues USING gin (culprit gin_trgm_ops);
