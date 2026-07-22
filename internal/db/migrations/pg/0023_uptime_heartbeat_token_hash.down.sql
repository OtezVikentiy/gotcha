-- Откат. Восстановить исходные heartbeat-токены нельзя — в heartbeat_token_hash
-- лежал необратимый sha256, — поэтому heartbeat_token воссоздаётся пустой
-- nullable-колонкой. Существующие heartbeat-мониторы останутся без токена и
-- потребуют пересоздания (ротации).
ALTER TABLE monitors ADD COLUMN heartbeat_token text UNIQUE;
ALTER TABLE monitors DROP COLUMN heartbeat_token_hash;
