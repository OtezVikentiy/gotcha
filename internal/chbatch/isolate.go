// Package chbatch — общие примитивы записи батчей в ClickHouse для писателей
// событий/трасс/метрик/профилей/uptime.
package chbatch

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// IsolatePoison пытается вставить rows одним батчем; при неудаче рекурсивно делит
// батч пополам, чтобы изолировать ряды, которые ClickHouse отвергает на data-level
// (битый enum, out-of-range значение, несовпадение типа и т.п.). Предикат isPoison
// классифицирует ошибку вставки: только «ядовитые» (серверные data/schema) ряды
// дропаются, транзиентные отказы (сеть/ctx/connection) НЕ теряются, а возвращаются
// в unresolved для обычного ретрая.
//
// Возвращает число дропнутых ядовитых рядов и unresolved — ряды, которые не
// удалось ни вставить, ни признать ядом (транзиент). Успешные под-батчи
// вставляются в процессе.
//
// Вызывать ТОЛЬКО как escalation после нескольких подряд-фейлов обычной вставки
// либо сразу при явно data-level ошибке: на чисто транзиентном отказе CH здесь
// батч раздробится на одиночные ряды (много лишних INSERT), но ничего не
// потеряется — все ряды вернутся через unresolved.
func IsolatePoison[T any](ctx context.Context, rows []T, insert func(context.Context, []T) error, isPoison func(error) bool) (dropped int, unresolved []T) {
	if len(rows) == 0 {
		return 0, nil
	}
	err := insert(ctx, rows)
	if err == nil {
		return 0, nil
	}
	if len(rows) == 1 {
		if isPoison(err) {
			return 1, nil // одиночный ряд отвергнут на data-level — это яд, дропаем
		}
		return 0, rows // транзиент — вернуть ряд на обычный ретрай, не терять
	}
	mid := len(rows) / 2
	dl, ul := IsolatePoison(ctx, rows[:mid], insert, isPoison)
	dr, ur := IsolatePoison(ctx, rows[mid:], insert, isPoison)
	// Свежий слайс: ul/ur — под-слайсы rows, склейка на месте затёрла бы данные.
	unresolved = append(append([]T(nil), ul...), ur...)
	return dl + dr, unresolved
}

// poisonCHCodes — коды серверных ошибок ClickHouse, означающих, что РЯД
// невставляем по своей природе (данные/тип/значение), и ретрай бесполезен —
// такой ряд изолируется и дропается. КРИТИЧНО не включать сюда транзиентные
// коды: перегрузка (MEMORY_LIMIT_EXCEEDED=241, TIMEOUT_EXCEEDED=159,
// TOO_MANY_SIMULTANEOUS_QUERIES=202, SOCKET_TIMEOUT=209, TOO_MANY_PARTS=252),
// сеть (NETWORK_ERROR=210) и схемные при rolling-миграции
// (NO_SUCH_COLUMN_IN_TABLE=16, UNKNOWN_TABLE=60, TABLE_IS_READ_ONLY=242) —
// они лечатся ретраем/восстановлением, и ряды НЕЛЬЗЯ терять как яд.
var poisonCHCodes = map[int32]bool{
	6:   true, // CANNOT_PARSE_TEXT
	26:  true, // CANNOT_PARSE_QUOTED_STRING
	27:  true, // CANNOT_PARSE_INPUT_ASSERTION_FAILED
	33:  true, // CANNOT_READ_ALL_DATA
	38:  true, // CANNOT_PARSE_DATE
	41:  true, // CANNOT_PARSE_DATETIME
	43:  true, // ILLEGAL_TYPE_OF_ARGUMENT
	53:  true, // TYPE_MISMATCH
	69:  true, // ARGUMENT_OUT_OF_BOUND
	70:  true, // CANNOT_CONVERT_TYPE
	72:  true, // CANNOT_PARSE_NUMBER
	117: true, // INCORRECT_DATA
	131: true, // TOO_LARGE_STRING_SIZE
	190: true, // SIZES_OF_ARRAYS_DONT_MATCH
	407: true, // DECIMAL_OVERFLOW
}

// IsServerDataError сообщает, является ли ошибка вставки НЕВСТАВЛЯЕМОЙ на
// data-level ClickHouse («яд»), которую бессмысленно ретраить (несовпадение
// типа, parse, out-of-range и т.п. — см. poisonCHCodes). Всё остальное —
// транзиент (false): ряды возвращаются в буфер под обычный ретрай, а не
// дропаются.
//
// Важно (RA-1): серверное исключение ClickHouse НЕ равно «яд» — перегрузочные
// и схемные коды транзиентны. Классифицируем строго по chErr.Code. Всё, что не
// распознано как data-level, считаем транзиентом (безопаснее не терять данные;
// клиентский Append-яд ограничивается overflow-trim буфера, как до RA-1).
func IsServerDataError(err error) bool {
	if err == nil {
		return false
	}
	// Дедлайн/отмена — всегда транзиент, даже если обёрнуты.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// Сервер ответил протокольным исключением: яд ТОЛЬКО для data-level кодов;
	// перегрузка/сеть/схема (не в whitelist) — транзиент.
	var chErr *clickhouse.Exception
	if errors.As(err, &chErr) {
		return poisonCHCodes[chErr.Code]
	}
	// Сеть/ctx/EOF/драйверные и всё прочее нераспознанное — транзиент.
	return false
}
