-- backward-compatible: yes (новая таблица)
CREATE TABLE IF NOT EXISTS logs (
    project_id      UInt64,
    timestamp       DateTime64(3),
    observed_ts     DateTime64(3),
    severity        LowCardinality(String),
    severity_number UInt8,
    severity_text   String,
    body            String,
    trace_id        String,
    span_id         String,
    log_attributes  Map(String, String),
    resource_attrs  Map(String, String),
    service         LowCardinality(String),
    environment     LowCardinality(String),
    INDEX idx_severity severity TYPE set(8) GRANULARITY 4,
    INDEX idx_trace    trace_id TYPE bloom_filter GRANULARITY 4
) ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (project_id, timestamp)
TTL toDateTime(timestamp) + INTERVAL 14 DAY;
