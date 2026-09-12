-- backward-compatible: yes (аддитивно — новые индексы на таблицах из 0092)
--
-- Обычный CREATE INDEX, не CONCURRENTLY: таблицы заведены той же парой миграций
-- и ещё не в релизе — исключительная блокировка на пустой таблице мгновенна.
CREATE INDEX log_saved_filters_project_id_idx ON log_saved_filters (project_id);
CREATE INDEX log_saved_filters_owner_user_id_idx ON log_saved_filters (owner_user_id)
    WHERE owner_user_id IS NOT NULL;
CREATE INDEX log_saved_filters_author_user_id_idx ON log_saved_filters (author_user_id)
    WHERE author_user_id IS NOT NULL;
CREATE INDEX log_default_filters_user_id_idx ON log_default_filters (user_id);
CREATE INDEX log_default_filters_filter_id_idx ON log_default_filters (filter_id);
