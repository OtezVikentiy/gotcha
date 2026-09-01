-- Откат снимает столбец целиком: типы ключей — знание этой версии, старому
-- бинарю оно не нужно, а сами ключи (public_key, revoked_at) не трогаются.
ALTER TABLE project_keys DROP COLUMN kind;
