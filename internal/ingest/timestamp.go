package ingest

import "time"

// max_partitions_per_insert_block (100) отбивает вставку при timestamp'ах из
// сотни разных месяцев в одном батче — запись встаёт для всего инстанса.
const (
	maxTimestampAge    = 90 * 24 * time.Hour
	maxTimestampFuture = 24 * time.Hour
)

func inRetentionWindow(ts, now time.Time) bool {
	return !ts.Before(now.Add(-maxTimestampAge)) && !ts.After(now.Add(maxTimestampFuture))
}

// Применяется к ошибкам: событие с кривым timestamp'ом — всё ещё настоящая
// ошибка. Транзакции при том же сбое отбрасываются целиком (см. ParseTransaction).
func clampToRetentionWindow(ts, now time.Time) time.Time {
	if lo := now.Add(-maxTimestampAge); ts.Before(lo) {
		return lo
	}
	if hi := now.Add(maxTimestampFuture); ts.After(hi) {
		return hi
	}
	return ts
}
