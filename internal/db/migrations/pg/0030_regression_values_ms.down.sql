-- Обратный пересчёт: значения длительности возвращаются в микросекунды.
UPDATE perf_regressions
SET baseline_value = baseline_value * 1000,
    peak_value     = peak_value * 1000,
    current_value  = current_value * 1000
WHERE metric = 'duration';
