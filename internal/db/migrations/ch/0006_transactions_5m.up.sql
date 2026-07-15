-- MV агрегирует только строки, вставленные ПОСЛЕ её создания, и читает
-- transactions — поэтому она идёт отдельной миграцией после 0003 и до
-- первой вставки.
CREATE MATERIALIZED VIEW IF NOT EXISTS transactions_5m
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(bucket)
ORDER BY (project_id, transaction, environment, bucket)
AS SELECT
    project_id,
    transaction,
    environment,
    toStartOfFiveMinutes(timestamp) AS bucket,
    countState() AS cnt,
    quantilesState(0.5, 0.75, 0.95, 0.99)(duration_us) AS dur,
    countIfState(status != 'ok') AS failures,
    sumState(toUInt64(duration_us)) AS total_us
FROM transactions
GROUP BY project_id, transaction, environment, bucket;
