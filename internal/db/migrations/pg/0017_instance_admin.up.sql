-- backward-compatible: yes (ADD COLUMN с дефолтом и частичный уникальный индекс по нему)
-- Первый зарегистрированный пользователь становится админом инстанса; частичный
-- уникальный индекс держит его ровно одним даже при гоночной регистрации.
ALTER TABLE users ADD COLUMN is_instance_admin boolean NOT NULL DEFAULT false;

CREATE UNIQUE INDEX one_instance_admin ON users ((is_instance_admin)) WHERE is_instance_admin;
