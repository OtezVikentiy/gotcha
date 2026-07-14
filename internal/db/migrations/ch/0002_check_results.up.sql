CREATE TABLE IF NOT EXISTS check_results (
    monitor_id  UInt64,
    project_id  UInt64,
    region      LowCardinality(String),
    timestamp   DateTime64(3),
    ok          UInt8,
    status_code UInt16,
    error       String,
    dns_ms      UInt32,
    connect_ms  UInt32,
    tls_ms      UInt32,
    ttfb_ms     UInt32,
    total_ms    UInt32,
    body_size   UInt32
) ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (monitor_id, region, timestamp)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;
