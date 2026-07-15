-- Этап 5 (social login): OAuth-only аккаунты не имеют пароля, поэтому
-- password_hash становится nullable. Внешние личности (провайдер+субъект)
-- живут в user_identities: один внешний субъект → ровно один аккаунт
-- (PK provider+subject), у аккаунта не более одной привязки на провайдера
-- (UNIQUE user_id+provider). Провижининг link-only/invite-gated: заводить
-- строки в users с NULL-паролем разрешено только по инвайту (см. web-слой).
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
