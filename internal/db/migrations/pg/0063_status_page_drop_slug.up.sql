-- backward-compatible: no  (DROP COLUMN — старый бинарь, читающий slug, сломается)
-- Код больше не читает и не пишет slug; легаси-адреса живут в status_page_redirects (301).

-- Замораживает slug'и, добавленные после 0062 при rolling-deploy — иначе их 301
-- потеряется после удаления колонки; на single-instance no-op.
INSERT INTO status_page_redirects (legacy_slug, status_page_id)
SELECT slug, id FROM status_pages WHERE slug IS NOT NULL
ON CONFLICT (legacy_slug) DO NOTHING;

ALTER TABLE status_pages DROP COLUMN slug;
