ALTER TABLE events
    ADD COLUMN IF NOT EXISTS trace_id String,
    ADD COLUMN IF NOT EXISTS span_id String;
