CREATE TABLE IF NOT EXISTS transactions (
    project_id  UInt64,
    trace_id    String,
    span_id     String,
    transaction String,
    op          LowCardinality(String),
    timestamp   DateTime64(3),
    duration_us UInt32,
    status      LowCardinality(String),
    environment LowCardinality(String),
    release     String,
    server_name String,
    user_id     String,
    tags        Map(String, String),
    source      LowCardinality(String)
) ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (project_id, transaction, timestamp)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;
