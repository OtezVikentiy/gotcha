CREATE TABLE IF NOT EXISTS events (
    event_id        UUID,
    project_id      UInt64,
    issue_id        UInt64,
    timestamp       DateTime64(3),
    level           LowCardinality(String),
    message         String,
    exception_type  String,
    exception_value String,
    stacktrace      String,
    environment     LowCardinality(String),
    release         String,
    server_name     String,
    sdk             String,
    user_id         String,
    user_ip         String,
    user_email      String,
    tags            Map(String, String),
    contexts        String
) ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (project_id, issue_id, timestamp)
TTL toDateTime(timestamp) + INTERVAL 90 DAY;
