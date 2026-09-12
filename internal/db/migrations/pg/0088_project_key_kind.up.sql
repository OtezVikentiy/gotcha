-- backward-compatible: yes (старый бинарь вставляет ключи без kind и получает legacy)
--
-- DEFAULT 'legacy' сознательно остаётся навсегда: снять его после раскатки сломало бы
-- откат релиза (старый CreateKey не знает про kind) — fail-closed держится в Go, не в БД.
ALTER TABLE project_keys ADD COLUMN kind text NOT NULL DEFAULT 'legacy'
    CHECK (kind IN ('browser','server','agent','legacy'));
