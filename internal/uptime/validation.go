package uptime

import "fmt"

// ValidationError — отказ проверки монитора с машинным кодом причины.
//
// Существует потому, что интерфейс показывал текст ошибки как есть:
// «монитор: uptime: invalid monitor: http url must be a valid http(s) URL» —
// слово «монитор» дважды на двух языках, имя Go-пакета и английская фраза
// посреди русской страницы. Перевести это было нечем: причина существовала
// только в виде строки, собранной для лога.
//
// Код — единственное, что пересекает границу пакета: сообщение собирает
// веб-слой на языке интерфейса. Field указывает, какое поле формы виновато, —
// без него сообщение висит над формой, а не у поля.
type ValidationError struct {
	// Code — машинная причина, она же суффикс i18n-ключа.
	Code string
	// Field — имя поля формы (name, url, interval_seconds, …). Пустое, если
	// ошибка не про конкретное поле.
	Field string
	// Args — подстановки в текст сообщения: пределы, имена, значения.
	Args map[string]string
}

func (e *ValidationError) Error() string {
	if len(e.Args) == 0 {
		return "uptime: invalid monitor: " + e.Code
	}
	return fmt.Sprintf("uptime: invalid monitor: %s %v", e.Code, e.Args)
}

// Unwrap возвращает ErrInvalidMonitor: весь существующий код проверяет
// принадлежность через errors.Is, и эта проверка обязана продолжать работать.
func (e *ValidationError) Unwrap() error { return ErrInvalidMonitor }

// invalid собирает ValidationError. args идут парами ключ-значение.
func invalid(field, code string, args ...string) error {
	e := &ValidationError{Code: code, Field: field}
	if len(args) >= 2 {
		e.Args = make(map[string]string, len(args)/2)
		for i := 0; i+1 < len(args); i += 2 {
			e.Args[args[i]] = args[i+1]
		}
	}
	return e
}
