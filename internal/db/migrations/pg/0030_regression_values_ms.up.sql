-- backward-compatible: no
--
-- Значения длительности в perf_regressions записывались в микросекундах:
-- пакетные запросы p95 эндпойнтов возвращали значение из transactions_5m без
-- конвертации, тогда как одиночные конвертировали. Показ и уведомления
-- трактовали их как миллисекунды, завышая ровно в тысячу раз.
--
-- Только metric = 'duration': web-vital'ы (lcp/fcp/ttfb/inp) уже записаны в
-- миллисекундах, CLS безразмерный — их правка испортила бы верные данные.
UPDATE perf_regressions
SET baseline_value = baseline_value / 1000,
    peak_value     = peak_value / 1000,
    current_value  = current_value / 1000
WHERE metric = 'duration';
