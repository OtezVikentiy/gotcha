-- backward-compatible: yes (новая таблица)
-- domain — email-домен организации (один на организацию), enforced=true запрещает
-- вход по паролю для его юзеров. default_role — роль JIT-провижининга.
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
