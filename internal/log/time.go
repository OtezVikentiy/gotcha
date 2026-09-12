package log

import (
	"math"
	"time"
)

// Партиционирование logs по toYYYYMM(timestamp): батч, размазанный по десяткам месяцев, упирается в
// ClickHouse max_partitions_per_insert_block и роняет весь приём — окно [now-90d, now+24h] ограничивает разброс.
const (
	maxLogTimestampAge    = 90 * 24 * time.Hour
	maxLogTimestampFuture = 24 * time.Hour
)

// ns==0/переполнение — нет надёжного времени, используем fallback. Иначе клампим к окну ретенции —
// лог с кривым временем всё ещё несёт полезное тело, терять его хуже, чем показать на границе окна.
func logTime(ns uint64, fallback time.Time) time.Time {
	if ns == 0 || ns > math.MaxInt64 {
		return fallback
	}
	ts := time.Unix(0, int64(ns)).UTC()
	if lo := fallback.Add(-maxLogTimestampAge); ts.Before(lo) {
		return lo
	}
	if hi := fallback.Add(maxLogTimestampFuture); ts.After(hi) {
		return hi
	}
	return ts
}
