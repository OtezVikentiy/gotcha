-- backward-compatible: yes (новый индекс)
--
-- Частичный host_incidents_one_open_idx (0066, WHERE status='open') не покрывает FK
-- host_id: планировщик не докажет, что удаляемые (в т.ч. resolved) строки попадают под предикат.
CREATE INDEX CONCURRENTLY IF NOT EXISTS host_incidents_host_id_idx
    ON host_incidents (host_id);
