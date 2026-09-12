-- Возвращает slug = public_id для строк без записи в status_page_redirects.
-- WHERE sp.slug IS NULL обязателен: старый бинарь мог создать страницу с настоящим slug
-- уже после up (DEFAULT на public_id) — без WHERE UPDATE затёр бы его молча.
UPDATE status_pages sp
SET slug = COALESCE(
    sp.slug,
    (SELECT r.legacy_slug FROM status_page_redirects r WHERE r.status_page_id = sp.id),
    sp.public_id)
WHERE sp.slug IS NULL;

ALTER TABLE status_pages ALTER COLUMN slug SET NOT NULL;
ALTER TABLE status_pages ADD CONSTRAINT status_pages_slug_key UNIQUE (slug);

DROP TABLE status_page_redirects;
ALTER TABLE status_pages DROP CONSTRAINT status_pages_public_id_key;
ALTER TABLE status_pages DROP COLUMN public_id;
