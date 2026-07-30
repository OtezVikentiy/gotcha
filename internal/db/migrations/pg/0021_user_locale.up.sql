-- backward-compatible: yes (ADD COLUMN с дефолтом)
ALTER TABLE users ADD COLUMN locale text NOT NULL DEFAULT '';
