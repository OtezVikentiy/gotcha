package chbatch

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// Вызывать только escalation после нескольких фейлов обычной вставки: при простое CH без яда
// батч раздробится в ~2N бесполезных INSERT (но ничего не потеряется — вернётся в unresolved).
func IsolatePoison[T any](ctx context.Context, rows []T, insert func(context.Context, []T) error, isPoison func(error) bool) (dropped int, unresolved []T) {
	if len(rows) == 0 {
		return 0, nil
	}
	// Контекст уже исчерпан — дробить бессмысленно и вредно.
	if ctx.Err() != nil {
		return 0, rows
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

// Коды, где ряд невставляем по своей природе. НЕ добавлять транзиентные (перегрузка, сеть, схема
// при rolling-миграции) — их лечит ретрай, а не дроп ряда как яда.
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

// Серверное исключение ClickHouse — не то же, что «яд»: перегрузочные и схемные коды транзиентны;
// нераспознанное считаем транзиентом, чтобы не терять данные по ошибке.
func IsServerDataError(err error) bool {
	if err == nil {
		return false
	}
	// Дедлайн/отмена — всегда транзиент, даже если обёрнуты.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var chErr *clickhouse.Exception
	if errors.As(err, &chErr) {
		return poisonCHCodes[chErr.Code]
	}
	return false
}
