-- backward-compatible: yes (ADD COLUMN)
ALTER TABLE events
    ADD COLUMN IF NOT EXISTS request String;
