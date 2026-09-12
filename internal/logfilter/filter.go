package logfilter

import (
	"strings"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

const (
	// Потолок длины имени фильтра в символах.
	maxNameLen = 60
	// Потолок числа условий в одном фильтре.
	maxPredicates = 20
	// Потолок личных фильтров одного пользователя в проекте.
	maxPersonalPerUser = 30
	// Отдельный потолок от личного: общий фильтр — ресурс проекта, а не одного автора, своя гонка.
	maxSharedPerProject = 30
	// Фильтр с другой версией не роняет чтение, а показывается неприменимым (Applicable=false) — формат мог
	// измениться, старая запись должна остаться видимой, не исчезнуть и не упасть на разборе.
	payloadVersion = 1
)

// OwnerUserID == nil означает общий фильтр проекта (см. Shared). AuthorUserID переживает удаление автора
// (ON DELETE SET NULL) — общий пресет показывается созданным удалённым пользователем, не исчезает.
type Filter struct {
	ID           int64
	ProjectID    int64
	OwnerUserID  *int64
	AuthorUserID *int64
	Name         string
	Predicates   []log.Predicate
	Applicable   bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// true для общего фильтра проекта (без личного владельца).
func (f Filter) Shared() bool { return f.OwnerUserID == nil }

// Отказ проверки с машинным кодом причины — по образцу internal/uptime/validation.go: доменный код,
// который веб-слой раскладывает в ключ "error.logfilter.<code>".
type ValidationError struct {
	Code  string
	Field string
}

func (e *ValidationError) Error() string { return "logfilter: " + e.Field + ": " + e.Code }

// Версионируемая форма хранения условий в jsonb. V != payloadVersion при чтении — формат неизвестен,
// фильтр возвращается с Applicable=false и пустыми Predicates, не роняет чтение.
type payload struct {
	V          int             `json:"v"`
	Predicates []log.Predicate `json:"predicates"`
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return &ValidationError{Code: "name_required", Field: "name"}
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return &ValidationError{Code: "name_too_long", Field: "name"}
	}
	return nil
}

// Потолок числа условий сюда НЕ входит — validatePredicateCount вызывается отдельно, ПОСЛЕ
// log.NormalizePredicates: до схлопывания дублей count ещё может быть завышен повторными кликами.
func validatePredicates(preds []log.Predicate) error {
	for _, p := range preds {
		if err := p.Validate(); err != nil {
			return &ValidationError{Code: "invalid_predicate", Field: "predicates"}
		}
	}
	return nil
}

// Вызывается ПОСЛЕ log.NormalizePredicates: до неё count может быть завышен повторными «исключить»
// по одному условию, что дало бы ложный too_many_predicates.
func validatePredicateCount(preds []log.Predicate) error {
	if len(preds) > maxPredicates {
		return &ValidationError{Code: "too_many_predicates", Field: "predicates"}
	}
	return nil
}
