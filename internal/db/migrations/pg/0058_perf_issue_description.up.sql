-- backward-compatible: yes (ADD COLUMN с дефолтом + backfill UPDATE)
-- Заголовок теперь строится на рендере по локали; title остаётся как fallback
-- чтения для строк, накопленных до этой миграции.
ALTER TABLE perf_issues ADD COLUMN description text NOT NULL DEFAULT '';
ALTER TABLE perf_issues ALTER COLUMN title SET DEFAULT '';

-- Backfill извлекает параметр из старых русских заголовков по двум префиксам;
-- не подошедшая строка остаётся с пустым description — рендер покажет сохранённый title.
UPDATE perf_issues SET description = substr(title, char_length('N+1 запросов: ') + 1)
    WHERE title LIKE 'N+1 запросов: %';
UPDATE perf_issues SET description = substr(title, char_length('Медленный запрос: ') + 1)
    WHERE title LIKE 'Медленный запрос: %';
