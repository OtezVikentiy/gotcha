-- Регрессии производительности: рост p95 эндпойнта или p75 web-vital'а над
-- скользящей базой моделируется как инцидент open/close — механика та же, что у
-- uptime-инцидентов (см. 0006_uptime, incidents_one_open_idx). target_kind:
-- 'endpoint_p95' | 'webvital_p75'; metric: 'duration' | 'lcp' | 'inp' | 'cls' |
-- 'fcp' | 'ttfb'.
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
-- на цель — не более одного открытого инцидента (приём incidents_one_open_idx).
CREATE UNIQUE INDEX perf_regressions_one_open_idx ON perf_regressions (project_id, target, metric) WHERE status = 'open';
CREATE INDEX perf_regressions_project_started_idx ON perf_regressions (project_id, started_at DESC);

-- Пороги регрессий — на проект, отдельной колонкой (не путать с
-- perf_detector_config этапа 3, это другой механизм). Дефолты в коде через
-- trace.RegressionConfigFromJSON: отсутствующий ключ → дефолт.
ALTER TABLE projects ADD COLUMN perf_regression_config jsonb NOT NULL DEFAULT '{}'::jsonb;
