-- backward-compatible: yes (новый индекс)
--
-- PostgreSQL не индексирует ссылающуюся сторону FK автоматически — без индекса
-- каскад ON DELETE CASCADE/SET NULL при удалении родителя идёт полным сканом дочерней
-- таблицы. Этот файл и 0038-0052 закрывают все непокрытые FK по одному индексу на файл.
CREATE INDEX CONCURRENTLY IF NOT EXISTS alert_channels_project_id_idx
    ON alert_channels (project_id);
