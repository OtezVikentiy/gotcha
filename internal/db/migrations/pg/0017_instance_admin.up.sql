-- PROD-B1: инстанс-админ. Первый зарегистрированный пользователь становится
-- админом инстанса (bootstrap). Частичный уникальный индекс гарантирует, что
-- инстанс-админ ровно один даже при гоночной первой регистрации: вторая вставка
-- с is_instance_admin=true упадёт на индексе.
ALTER TABLE users ADD COLUMN is_instance_admin boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX one_instance_admin ON users ((is_instance_admin)) WHERE is_instance_admin;
