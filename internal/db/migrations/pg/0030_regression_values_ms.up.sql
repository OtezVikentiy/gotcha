-- backward-compatible: no
--
-- Значения duration хранились в микросекундах, показ трактовал их как миллисекунды (x1000).
-- Правится только metric='duration' — web-vitals уже в мс, CLS безразмерный.
UPDATE perf_regressions
SET baseline_value = baseline_value / 1000,
    peak_value     = peak_value / 1000,
    current_value  = current_value / 1000
WHERE metric = 'duration';
