package ingest

import "time"

// Окно допустимых timestamp'ов SDK: [now-90d, now+1d].
//
// Зачем окно вообще: CH-таблицы events/transactions/spans партиционированы по
// toYYYYMM(timestamp), а ClickHouse отбивает INSERT-блок, затрагивающий больше
// max_partitions_per_insert_block партиций (по умолчанию 100) — «Code: 252,
// Too many partitions for single INSERT block». Публичный DSN-ключ по замыслу
// лежит внутри клиентского приложения, так что кто угодно может прислать
// envelope с сотней item'ов, у которых timestamp'ы разнесены по разным
// месяцам. Такая пачка попадает в один батч писателя, вставка падает, батч
// возвращается в голову буфера — и КАЖДЫЙ следующий флаш снова упирается в те
// же строки: запись встаёт для всего инстанса (для всех организаций). Поэтому
// подобные timestamp'ы не должны доезжать до писателя вообще.
//
// Почему именно 90d/1d: у events/transactions TTL 90 дней (у spans — 30), всё,
// что старше, ClickHouse всё равно выбросит; сутки вперёд — запас на кривые
// часы клиента. Внутри окна не больше пяти месячных партиций, до лимита в 100
// не дотянуться.
const (
	maxTimestampAge    = 90 * 24 * time.Hour
	maxTimestampFuture = 24 * time.Hour
)

// inRetentionWindow — попадает ли ts в окно [now-90d, now+1d].
func inRetentionWindow(ts, now time.Time) bool {
	return !ts.Before(now.Add(-maxTimestampAge)) && !ts.After(now.Add(maxTimestampFuture))
}

// clampToRetentionWindow подтягивает ts к ближайшей границе окна. Применяется к
// ОШИБКАМ (см. parseTimestamp): событие с кривым timestamp'ом — всё ещё
// настоящая ошибка со стектрейсом, терять её из-за сбитых часов клиента хуже,
// чем показать её на границе окна. Транзакции, наоборот, отбрасываются целиком
// (см. ParseTransaction): это чистая телеметрия, а сдвинутый timestamp сломал
// бы длительности.
func clampToRetentionWindow(ts, now time.Time) time.Time {
	if lo := now.Add(-maxTimestampAge); ts.Before(lo) {
		return lo
	}
	if hi := now.Add(maxTimestampFuture); ts.After(hi) {
		return hi
	}
	return ts
}
