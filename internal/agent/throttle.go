// throttle.go — троттлинг дорогих проб без изменения контракта Probes
// (probes.go/collect.go не трогаем): обёртка кэширует последний снимок и
// обращается к боевой пробе не чаще заданного интервала.
package agent

import "time"

// procsProbeInterval — минимальный интервал между реальными обращениями к
// пробе процессов. process.Processes()+Status() — самая дорогая часть сбора
// (ops-MED, аудит эксплуатации: ~66мс на 625 процессов, то есть почти весь
// 65-мс тик; на хосте с 10 000 процессов — около секунды CPU и ~20 тысяч
// файловых операций каждые 30с). Раскладка processes.count по статусам не
// обязана обновляться так же часто, как остальные метрики: между реальными
// опросами отдаём последний известный снимок — устаревшая на десятки секунд
// раскладка лучше, чем нагрузка на каждый тик.
const procsProbeInterval = 60 * time.Second

// throttledProcs оборачивает пробу процессов кэшем по времени: реальный
// вызов probe — не чаще interval (по показаниям now), между вызовами
// возвращается последний результат (включая последнюю ошибку, если проба
// упала — вызывающий код уже умеет переживать её отказ, см. Collect).
// now передаётся параметром (а не time.Now внутри) — детерминируемые тесты.
func throttledProcs(probe func() (map[string]int, error), now func() time.Time, interval time.Duration) func() (map[string]int, error) {
	var last time.Time
	var cached map[string]int
	var cachedErr error
	return func() (map[string]int, error) {
		n := now()
		if !last.IsZero() && n.Sub(last) < interval {
			return cached, cachedErr
		}
		last = n
		cached, cachedErr = probe()
		return cached, cachedErr
	}
}
