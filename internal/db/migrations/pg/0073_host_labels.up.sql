-- backward-compatible: yes (ADD COLUMN с дефолтами)
-- Метки хоста (B1): окружение и роль из resource-атрибутов телеметрии
-- (deployment.environment / host.role). NOT NULL DEFAULT '' — «метка неизвестна»
-- моделируется пустой строкой, как agent_version через COALESCE.
ALTER TABLE hosts ADD COLUMN environment TEXT NOT NULL DEFAULT '';
ALTER TABLE hosts ADD COLUMN role TEXT NOT NULL DEFAULT '';
