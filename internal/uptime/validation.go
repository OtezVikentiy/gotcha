package uptime

import "fmt"

// Code — единственное, что пересекает границу пакета; сообщение на языке
// интерфейса собирает веб-слой. Field — какое поле формы виновато.
type ValidationError struct {
	// машинная причина, она же суффикс i18n-ключа.
	Code string
	// имя поля формы; пустое, если ошибка не про конкретное поле.
	Field string
	// подстановки в текст сообщения: пределы, имена, значения.
	Args map[string]string
}

func (e *ValidationError) Error() string {
	if len(e.Args) == 0 {
		return "uptime: invalid monitor: " + e.Code
	}
	return fmt.Sprintf("uptime: invalid monitor: %s %v", e.Code, e.Args)
}

// весь код проверяет принадлежность через errors.Is(ErrInvalidMonitor) — это
// обязано продолжать работать.
func (e *ValidationError) Unwrap() error { return ErrInvalidMonitor }

// args идут парами ключ-значение.
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
