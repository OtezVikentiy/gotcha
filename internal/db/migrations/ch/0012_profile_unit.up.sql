-- backward-compatible: yes (ADD COLUMN с дефолтом)
-- Единица SampleType.Unit ('nanoseconds'/'bytes'/'count') раньше терялась — UI гадал
-- по таблице тип→единица. DEFAULT '' — для старых строк UI возвращается к той догадке.
ALTER TABLE profile_samples ADD COLUMN unit LowCardinality(String) DEFAULT '';
