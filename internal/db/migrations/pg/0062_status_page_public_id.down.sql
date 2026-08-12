-- Contract-обратно: вернуть глобальный уникальный slug. Данные не потеряны —
-- legacy_slug заморожен в status_page_redirects. Строкам, созданным уже в
-- новой модели (без записи в redirects), возвращаем slug = public_id
-- (уникален, валиден на уровне БД — CHECK на slug нет).
--
-- WHERE sp.slug IS NULL обязателен: у public_id в up.sql есть DEFAULT именно
-- затем, чтобы старый бинарь мог создавать страницы уже ПОСЛЕ up, явно
-- указывая slug, но не зная про public_id (INSERT из internal/uptime/
-- statuspage.go:74-77). У таких строк slug настоящий, непустой, а записи в
-- status_page_redirects нет — она пишется только для строк, существовавших
-- ДО up. Безусловный UPDATE (без WHERE) затирал бы их настоящий slug на
-- public_id, теряя данные молча: COALESCE(NULL из подзапроса, sp.public_id)
-- срабатывал бы для НЕПУСТОГО slug точно так же, как для пустого.
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
