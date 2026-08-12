-- backward-compatible: no  (DROP COLUMN — старый бинарь, читающий slug, сломается)
-- Contract: код больше не читает и не пишет slug (задачи T2-T4). Удаляем
-- колонку; легаси-адреса живут в status_page_redirects (для 301), не здесь.

-- Заморозить любые slug'и, появившиеся после 0062 (старый бинарь на переходном
-- окне при rolling-deploy) — иначе их публичный адрес /status/<slug> потеряет
-- 301 после удаления колонки. Для single-instance это no-op (окна нет), но
-- делает миграцию корректной под любую модель деплоя. ON CONFLICT — те, что
-- уже заморожены 0062, пропускаем.
INSERT INTO status_page_redirects (legacy_slug, status_page_id)
SELECT slug, id FROM status_pages WHERE slug IS NOT NULL
ON CONFLICT (legacy_slug) DO NOTHING;

ALTER TABLE status_pages DROP COLUMN slug;
