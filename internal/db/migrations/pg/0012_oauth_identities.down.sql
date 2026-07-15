-- Откат корректен лишь пока нет аккаунтов с NULL-паролем (OAuth-only): иначе
-- SET NOT NULL упадёт. Для dev/rollback приемлемо.
DROP TABLE user_identities;
ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;
