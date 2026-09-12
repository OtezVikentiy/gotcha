package agent

import "time"

// process.Processes()+Status() — самая дорогая часть сбора (~66мс на 625
// процессов, почти весь 65-мс тик); устаревший снимок лучше нагрузки на тик.
const procsProbeInterval = 60 * time.Second

// Кэширует по времени: реальный вызов probe не чаще interval, включая
// последнюю ошибку. now — параметром для детерминируемых тестов.
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
