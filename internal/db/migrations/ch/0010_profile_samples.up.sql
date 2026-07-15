CREATE TABLE profile_samples (
    project_id   UInt64,
    profile_type LowCardinality(String),
    service      LowCardinality(String),
    environment  LowCardinality(String),
    transaction  String,
    platform     LowCardinality(String),
    ts           DateTime64(3, 'UTC'),
    stack        Array(String),
    value        UInt64
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (project_id, profile_type, service, ts)
TTL toDateTime(ts) + INTERVAL 7 DAY;
