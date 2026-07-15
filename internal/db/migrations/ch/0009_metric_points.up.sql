CREATE TABLE metric_points (
    project_id      UInt64,
    name            String,
    type            LowCardinality(String),
    unit            LowCardinality(String),
    service         LowCardinality(String),
    environment     LowCardinality(String),
    attributes      Map(String, String),
    ts              DateTime64(3, 'UTC'),
    value           Float64,
    count           UInt64,
    bucket_counts   Array(UInt64),
    explicit_bounds Array(Float64),
    monotonic       UInt8,
    temporality     LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(ts)
ORDER BY (project_id, name, service, ts)
TTL toDateTime(ts) + INTERVAL 30 DAY;
