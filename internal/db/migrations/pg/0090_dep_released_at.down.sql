-- Откат — симметричный DROP, колонки аддитивны, данных с самостоятельной
-- ценностью не теряют (момент снятия подавления восстановим из истории
-- инцидента, если понадобится).
ALTER TABLE host_incidents DROP COLUMN dep_released_at;
ALTER TABLE incidents      DROP COLUMN dep_released_at;
