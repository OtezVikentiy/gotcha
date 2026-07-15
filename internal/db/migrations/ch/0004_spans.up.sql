CREATE TABLE IF NOT EXISTS spans (
    project_id       UInt64,
    trace_id         String,
    span_id          String,
    parent_span_id   String,
    transaction      String,
    op               LowCardinality(String),
    description      String,
    description_hash UInt64,
    timestamp        DateTime64(3),
    duration_us      UInt32,
    status           LowCardinality(String),
    environment      LowCardinality(String),
    data             String,
    source           LowCardinality(String)
) ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (project_id, trace_id, timestamp)
TTL toDateTime(timestamp) + INTERVAL 30 DAY;
