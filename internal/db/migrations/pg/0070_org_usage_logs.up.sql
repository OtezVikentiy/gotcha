-- backward-compatible: yes
-- C1 (логи): квота логов (per-request счётчик, как метрики/профили) и её
-- предобраз для checkAndCount (см. 0057_org_usage_preimage — logs_count_before
-- обязателен по той же причине: без него первое списание на непустой БД
-- падает на RETURNING несуществующей колонки).
ALTER TABLE org_usage
    ADD COLUMN IF NOT EXISTS logs_count        bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS logs_count_before bigint NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dropped_logs      bigint NOT NULL DEFAULT 0;
ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS log_quota bigint NOT NULL DEFAULT 0;
