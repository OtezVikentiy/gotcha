-- backward-compatible: yes (новый индекс)
--
-- Покрытие ссылающейся стороны FK host_incidents.host_id (см. докблок
-- 0037_alert_channels_project_id_idx.up.sql про находку №45: без индекса
-- каскад ON DELETE CASCADE при удалении хоста идёт полным сканом). Частичный
-- host_incidents_one_open_idx (0066, WHERE status='open') этот случай не
-- покрывает: планировщик не может доказать, что удаляемые строки попадают
-- под чужой предикат, и для resolved-строк сканить всё равно пришлось бы —
-- нужен отдельный полный индекс на host_id (сторож fkindex это требует).
CREATE INDEX CONCURRENTLY IF NOT EXISTS host_incidents_host_id_idx
    ON host_incidents (host_id);
