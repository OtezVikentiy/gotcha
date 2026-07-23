ALTER TABLE events
    ADD COLUMN IF NOT EXISTS breadcrumbs String;
