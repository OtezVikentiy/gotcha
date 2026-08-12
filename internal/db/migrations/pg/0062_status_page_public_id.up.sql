-- backward-compatible: no  (DROP CONSTRAINT status_pages_slug_key)
--
-- public_id получает DEFAULT (см. ниже) именно для того, чтобы INSERT
-- старого бинаря — internal/uptime/statuspage.go:74, явно перечисляет
-- (project_id, slug, title, description, enabled) и НЕ знает про public_id —
-- не падал на NOT NULL: колонке, не упомянутой в INSERT, подставляется
-- DEFAULT. Без этого DEFAULT миграция ломала бы вообще любое создание
-- статус-страницы старым бинарём на переходном окне деплоя.
--
-- Тем не менее маркер — no, и не только из-за буквальной формы: DROP
-- CONSTRAINT status_pages_slug_key ниже снимает единственную защиту от гонки,
-- которой старый бинарь реально пользовался — при одновременном создании
-- двух страниц с одинаковым slug раньше один INSERT падал unique-violation,
-- теперь оба пройдут и получится дубликат. Плюс страж
-- (internal/guards/migrations_test.go, destructiveForms) распознаёт форму
-- DROP CONSTRAINT буквально, без учёта направления — он уже один раз
-- пропустил разрушительную DROP CONSTRAINT в pg/0029, замаскированную под
-- безопасную, маркер там совпал с реальностью случайно, не по проверке.
-- Ослабленный инвариант не откатывается автоматически, если потребуется
-- откат релиза через эту версию — см. down.sql.
--
-- Expand: публичный адрес статус-страницы переходит с глобального slug на
-- непрозрачный ключ. Здесь только ДОБАВЛЯЕМ (slug остаётся, но ослаблен),
-- чтобы существующий код не сломался; удаление slug — миграция 0063.
--
-- gen_random_bytes() из pgcrypto здесь недоступен: эта инсталляция pgcrypto
-- не подключает (см. 0023_uptime_heartbeat_token_hash.up.sql — там ради
-- одной миграции решили не тянуть расширение и просто оставили колонку
-- NULL). Здесь так не выйдет: public_id ниже NOT NULL сразу для ВСЕХ строк —
-- и существующих (нужен backfill), и будущих от старого бинаря (нужен
-- DEFAULT). Вместо расширения берём gen_random_uuid() — он встроен в ядро
-- Postgres с v13, расширений не требует. DEFAULT волатилен (вызов функции),
-- поэтому ADD COLUMN ниже перевычисляет его ОТДЕЛЬНО для каждой уже
-- существующей строки при перезаписи таблицы — отдельный backfill UPDATE не
-- нужен. 24 hex-символа (совпадает с форматом encode(gen_random_bytes(12),
-- 'hex'), который ждут задачи T2/T3) — первые 24 из 32 hex-цифр UUID; в их
-- числе version/variant-полубайты (2 из 24 не полностью случайны), но для
-- непрозрачного идентификатора публичной страницы (не auth-секрет вроде
-- heartbeat-токена) запас энтропии избыточен и это не имеет значения.
ALTER TABLE status_pages ADD COLUMN public_id text NOT NULL
    DEFAULT ('p_' || substr(replace(gen_random_uuid()::text, '-', ''), 1, 24));
ALTER TABLE status_pages ADD CONSTRAINT status_pages_public_id_key UNIQUE (public_id);

-- Заморозка старых адресов для 301-редиректа. Существующие slug'и уникальны
-- (снимаем ровно этот constraint ниже), поэтому переносятся без коллизий.
CREATE TABLE status_page_redirects (
    legacy_slug    text PRIMARY KEY,
    status_page_id bigint NOT NULL REFERENCES status_pages(id) ON DELETE CASCADE,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX status_page_redirects_page_idx ON status_page_redirects (status_page_id);

INSERT INTO status_page_redirects (legacy_slug, status_page_id)
SELECT slug, id FROM status_pages;

-- Ослабляем slug: больше не глобально-уникален и не обязателен (новые
-- страницы его не задают). Колонку удалит 0063, когда код перестанет её читать.
ALTER TABLE status_pages DROP CONSTRAINT status_pages_slug_key;
ALTER TABLE status_pages ALTER COLUMN slug DROP NOT NULL;
