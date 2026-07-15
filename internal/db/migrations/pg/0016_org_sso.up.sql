-- Этап 10 (enterprise-SSO): организация привязывает свой OIDC-IdP (конфиг в БД,
-- не env). domain — email-домен организации (identifier-first вход + принуждение);
-- один домен за одной организацией. enforced=true → юзеры домена обязаны входить
-- через SSO (пароль не принимается). default_role — роль JIT-провижинингованного
-- участника.
CREATE TABLE org_sso (
    org_id        bigint PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    issuer        text NOT NULL,
    client_id     text NOT NULL,
    client_secret text NOT NULL,
    domain        citext NOT NULL UNIQUE,
    default_role  text NOT NULL DEFAULT 'member' CHECK (default_role IN ('admin','member')),
    enforced      boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now()
);
