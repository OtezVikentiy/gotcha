-- backward-compatible: yes (ADD COLUMN с дефолтом)
ALTER TABLE users ADD COLUMN theme text NOT NULL DEFAULT '';
