-- backward-compatible: no  (heartbeat_token удалён из monitors — старый бинарь ищет heartbeat-мониторы по этой колонке)
-- Бэкфилл невозможен без pgcrypto (не подключено, см. 0001_init) — sha256 в чистом SQL не посчитать.
-- heartbeat_token_hash остаётся NULL для старых мониторов: их heartbeat-URL перестанут
-- находиться (ByHeartbeatToken ищет по хешу) — такие мониторы нужно пересоздать.
ALTER TABLE monitors ADD COLUMN heartbeat_token_hash bytea UNIQUE;
ALTER TABLE monitors DROP COLUMN heartbeat_token;
