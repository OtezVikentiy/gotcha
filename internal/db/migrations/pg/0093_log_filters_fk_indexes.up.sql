-- backward-compatible: yes (аддитивно — новые индексы на таблицах,
-- заведённых миграцией 0092)
--
-- Правка находки финального ревью C13 (internal/guards,
-- TestForeignKeysHaveCoveringIndex): пять внешних ключей
-- log_saved_filters/log_default_filters (0092) не покрыты индексом —
-- каскадное удаление проекта/пользователя и JOIN по сохранённому фильтру
-- шли бы последовательным сканом. Новая миграция, а не правка уже
-- написанной 0092: та могла быть применена на стендах.
--
-- Обычный CREATE INDEX, не CONCURRENTLY: обе таблицы заведены этой же
-- парой миграций и ещё не выпущены в релиз — исключительная блокировка на
-- пустой таблице мгновенна, в отличие от 0031/0089, где индекс добавлялся
-- на уже боевую таблицу (issues).
--
-- owner_user_id/author_user_id — частичные индексы (WHERE ... IS NOT NULL):
-- обе колонки nullable (общий фильтр не имеет владельца, автор общего
-- фильтра переживает удаление своего аккаунта — ON DELETE SET NULL), и
-- ровно этот вид частичного индекса каскад/JOIN по непустому значению
-- покрывает не хуже полного, будучи меньше. project_id/user_id/filter_id —
-- NOT NULL, обычные полные индексы.
CREATE INDEX log_saved_filters_project_id_idx ON log_saved_filters (project_id);
CREATE INDEX log_saved_filters_owner_user_id_idx ON log_saved_filters (owner_user_id)
    WHERE owner_user_id IS NOT NULL;
CREATE INDEX log_saved_filters_author_user_id_idx ON log_saved_filters (author_user_id)
    WHERE author_user_id IS NOT NULL;
CREATE INDEX log_default_filters_user_id_idx ON log_default_filters (user_id);
CREATE INDEX log_default_filters_filter_id_idx ON log_default_filters (filter_id);
