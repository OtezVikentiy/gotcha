-- backward-compatible: no  (password_hash становится nullable: строки, заведённые новым кодом (OAuth-аккаунты без пароля), старый бинарь прочитать не сможет)
-- Внешние личности живут в user_identities: один субъект — один аккаунт (PK provider+subject),
-- не более одной привязки на провайдера (UNIQUE user_id+provider); провижининг только по инвайту.
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

CREATE TABLE user_identities (
    user_id    bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider   text   NOT NULL,
    subject    text   NOT NULL,
    email      citext,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (provider, subject),
    UNIQUE (user_id, provider)
);
CREATE INDEX user_identities_user_id_idx ON user_identities (user_id);
