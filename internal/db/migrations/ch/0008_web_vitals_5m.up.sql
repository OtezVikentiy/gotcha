-- backward-compatible: yes (новое материализованное представление)
-- MV читает только новые строки — идёт после 0007 (measurements) и до первой вставки.
-- mapContains в фильтре обязателен: отсутствующий ключ иначе даст 0.0 и испортит p75.
CREATE MATERIALIZED VIEW IF NOT EXISTS web_vitals_5m
ENGINE = AggregatingMergeTree
PARTITION BY toYYYYMM(bucket)
ORDER BY (project_id, transaction, environment, bucket)
AS SELECT
    project_id,
    transaction,
    environment,
    toStartOfFiveMinutes(timestamp) AS bucket,
    quantilesStateIf(0.75)(measurements['lcp'],  mapContains(measurements, 'lcp'))  AS lcp,
    quantilesStateIf(0.75)(measurements['inp'],  mapContains(measurements, 'inp'))  AS inp,
    quantilesStateIf(0.75)(measurements['cls'],  mapContains(measurements, 'cls'))  AS cls,
    quantilesStateIf(0.75)(measurements['fcp'],  mapContains(measurements, 'fcp'))  AS fcp,
    quantilesStateIf(0.75)(measurements['ttfb'], mapContains(measurements, 'ttfb')) AS ttfb,
    countStateIf(mapContains(measurements, 'lcp'))  AS lcp_count,
    countStateIf(mapContains(measurements, 'inp'))  AS inp_count,
    countStateIf(mapContains(measurements, 'cls'))  AS cls_count,
    countStateIf(mapContains(measurements, 'fcp'))  AS fcp_count,
    countStateIf(mapContains(measurements, 'ttfb')) AS ttfb_count
FROM transactions
GROUP BY project_id, transaction, environment, bucket
