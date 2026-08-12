-- Восстановить колонку slug до состояния после 0062 (nullable, без UNIQUE).
-- Значения — из замороженных редиректов; строкам новой модели (без записи в
-- redirects) — из public_id (уникальны, валидны на уровне БД). Заполняем ВСЕ
-- строки (slug только что добавлен, везде NULL — сохранять нечего, поэтому
-- безусловный UPDATE здесь корректен, в отличие от 0062.down).
ALTER TABLE status_pages ADD COLUMN slug text;
UPDATE status_pages sp
SET slug = COALESCE(
    (SELECT r.legacy_slug FROM status_page_redirects r WHERE r.status_page_id = sp.id),
    sp.public_id);
