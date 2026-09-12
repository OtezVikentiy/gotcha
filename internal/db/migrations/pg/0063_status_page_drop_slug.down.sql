-- Значения — из public_id (уникален, валиден); столбец только что добавлен и везде
-- NULL, поэтому безусловный UPDATE корректен, в отличие от 0062.down.
ALTER TABLE status_pages ADD COLUMN slug text;
UPDATE status_pages sp
SET slug = COALESCE(
    (SELECT r.legacy_slug FROM status_page_redirects r WHERE r.status_page_id = sp.id),
    sp.public_id);
