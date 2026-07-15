-- MV агрегирует только строки, вставленные ПОСЛЕ её создания, и читает
-- transactions.measurements — поэтому она идёт отдельной миграцией после
-- 0007 (колонка measurements уже есть) и до первой вставки.
--
-- p75 — стандарт агрегации Web Vitals у Google, отсюда фиксированный уровень
-- квантиля 0.75. mapContains в фильтре обязателен: measurements['lcp'] для
-- отсутствующего ключа вернул бы 0.0 и испортил бы p75.
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
