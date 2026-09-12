-- backward-compatible: no  (DROP CONSTRAINT status_pages_slug_key)
--
-- public_id получает DEFAULT, чтобы INSERT старого бинаря (не знающего о колонке) не падал
-- на NOT NULL при создании страницы на переходном окне деплоя.
--
-- Маркер — no не только по букве: DROP CONSTRAINT status_pages_slug_key ниже снимает
-- единственную защиту от гонки создания двух страниц с одинаковым slug. Страж
-- destructiveForms распознаёт DROP CONSTRAINT буквально, без учёта направления — уже
-- пропускал разрушительную форму в pg/0029, замаскированную под безопасную.
--
-- gen_random_uuid() вместо pgcrypto (не подключено в этой инсталляции, см. 0023) — встроен
-- в ядро с PG13. DEFAULT волатилен, поэтому ADD COLUMN сам бэкфиллит каждую строку.
-- 24 hex-символа — формат encode(gen_random_bytes(12), 'hex'), который код ожидает; избыток
-- энтропии от version/variant-полубайт UUID не важен для непрозрачного, не auth-идентификатора.
ALTER TABLE status_pages ADD COLUMN public_id text NOT NULL
    DEFAULT ('p_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 24));
ALTER TABLE status_pages ADD CONSTRAINT status_pages_public_id_key UNIQUE (public_id);

-- Существующие slug'и уникальны (снимаем ровно этот constraint ниже) — переносятся без коллизий.
CREATE TABLE status_page_redirects (
    legacy_slug    text PRIMARY KEY,
    status_page_id bigint NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX status_page_redirects_page_idx ON status_page_redirects (status_page_id);

INSERT INTO status_page_redirects (legacy_slug, status_page_id)
SELECT slug, id FROM status_pages;

-- Колонку удалит 0063, когда код перестанет её читать.
ALTER TABLE status_pages DROP CONSTRAINT status_pages_slug_key;
ALTER TABLE status_pages ALTER COLUMN slug DROP NOT NULL;
