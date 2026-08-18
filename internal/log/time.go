package log

import (
	"math"
	"time"
)

// Окно допустимых timestamp'ов логов: [now-90d, now+24h]. Константы-паритет с
// internal/ingest/timestamp.go (maxTimestampAge/maxTimestampFuture) и
// internal/metric/parse.go (maxPointAge/maxPointFuture) — локальная копия,
// т.к. эти пакеты друг у друга не переиспользуются (см. capRunes в sanitize.go).
//
// Зачем вообще нужно окно: таблица logs партиционирована по toYYYYMM(timestamp)
// (см. спеку C1 §1.1). Один запрос с записями, размазанными по десяткам
// месяцев, упирается в ClickHouse-лимит max_partitions_per_insert_block —
// вставка всего батча падает и встаёт в голове буфера писателя, останавливая
// приём логов для всего инстанса. Публичный DSN-ключ лежит в клиентском
// приложении, так что такой батч может прислать кто угодно.
const (
	maxLogTimestampAge    = 90 * 24 * time.Hour
	maxLogTimestampFuture = 24 * time.Hour
)

// logTime переводит наносекунды OTLP в момент времени записи лога. ns==0 (поле
// не заполнено) или переполнение int64 — событие без надёжного времени,
// используем fallback (серверное время приёма). Иначе — клампим к окну
// ретенции: в отличие от метрик (metric/parse.go pointTime отбрасывает точку
// целиком), лог с кривым временем всё ещё несёт полезное тело/severity —
// терять запись из-за сбитых часов клиента хуже, чем показать её на границе
// окна (та же логика, что ingest.clampToRetentionWindow для событий-ошибок).
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
