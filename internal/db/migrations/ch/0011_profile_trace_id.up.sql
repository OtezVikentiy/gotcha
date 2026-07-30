-- backward-compatible: yes (ADD COLUMN с дефолтом)
ALTER TABLE profile_samples ADD COLUMN trace_id String DEFAULT '';
