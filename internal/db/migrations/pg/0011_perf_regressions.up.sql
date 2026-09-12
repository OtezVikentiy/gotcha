-- backward-compatible: yes (новая таблица и ADD COLUMN с дефолтом)
-- Инцидент open/close, как у uptime (см. 0006_uptime, incidents_one_open_idx).
CREATE TABLE perf_regressions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    target_kind text NOT NULL,          -- 'endpoint_p95' | 'webvital_p75'
    target text NOT NULL,               -- имя транзакции/страницы
    metric text NOT NULL,               -- 'duration' | 'lcp' | 'inp' | 'cls' | 'fcp' | 'ttfb'
    status text NOT NULL DEFAULT 'open' CHECK (status IN ('open','resolved')),
    baseline_value double precision NOT NULL,
    peak_value double precision NOT NULL,
    current_value double precision NOT NULL,
    started_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz,
    notified_open boolean NOT NULL DEFAULT false,
    notified_close boolean NOT NULL DEFAULT false
);
CREATE UNIQUE INDEX perf_regressions_one_open_idx ON perf_regressions (project_id, target, metric) WHERE status = 'open';
CREATE INDEX perf_regressions_project_started_idx ON perf_regressions (project_id, started_at DESC);

-- Не путать с perf_detector_config — другой механизм. Дефолты в коде: отсутствующий ключ → дефолт.
ALTER TABLE projects ADD COLUMN perf_regression_config jsonb NOT NULL DEFAULT '{}'::jsonb;
