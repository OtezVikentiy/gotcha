-- backward-compatible: yes (новый индекс)

CREATE INDEX CONCURRENTLY IF NOT EXISTS check_queue_leased_by_idx
    ON check_queue (leased_by) WHERE leased_by IS NOT NULL;
