-- backward-compatible: yes (ADD COLUMN с дефолтом + backfill UPDATE)
-- №132: заголовок perf-находки строится на рендере по локали смотрящего.
-- В БД остаются вид (kind) и параметр (description); title больше не
-- пишется — DEFAULT '' страхует INSERT детектора, сама колонка остаётся
-- как fallback чтения для строк, накопленных до этой миграции.
ALTER TABLE perf_issues ADD COLUMN description text NOT NULL DEFAULT '';
ALTER TABLE perf_issues ALTER COLUMN title SET DEFAULT '';

-- Backfill: параметр извлекается из старых русских заголовков по двум
-- известным префиксам детекторов. У http_flood параметра нет (culprit уже
-- отдельная колонка). Строка, не подошедшая ни под один префикс, остаётся
-- с пустым description — для неё рендер показывает сохранённый title.
UPDATE perf_issues SET description = substr(title, char_length('N+1 запросов: ') + 1)
    WHERE title LIKE 'N+1 запросов: %';
UPDATE perf_issues SET description = substr(title, char_length('Медленный запрос: ') + 1)
    WHERE title LIKE 'Медленный запрос: %';
