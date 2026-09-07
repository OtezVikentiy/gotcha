// Package logfilter хранит сохранённые фильтры логов — личные и общие
// по проекту — а также фильтр по умолчанию для пары (проект, пользователь).
package logfilter

import (
	"strings"
	"time"
	"unicode/utf8"

	"gitflic.ru/otezvikentiy/gotcha/internal/log"
)

const (
	// maxNameLen — потолок длины имени фильтра в символах.
	maxNameLen = 60
	// maxPredicates — потолок числа условий в одном фильтре.
	maxPredicates = 20
	// maxPersonalPerUser — потолок личных фильтров одного пользователя
	// в проекте.
	maxPersonalPerUser = 30
	// maxSharedPerProject — потолок общих фильтров проекта. Отдельный
	// от личного: общий фильтр — общий ресурс проекта, а не одного автора,
	// у него свой потолок и своя гонка.
	maxSharedPerProject = 30
	// payloadVersion — текущая версия формата payload. Фильтр с другой
	// версией не роняет чтение, а показывается неприменимым (Applicable=false):
	// формат мог измениться в новой версии продукта, и старая запись должна
	// остаться видимой в списке, а не исчезнуть или упасть на разборе.
	payloadVersion = 1
)

// Filter — сохранённый фильтр логов. OwnerUserID == nil означает общий
// фильтр проекта (см. Shared). AuthorUserID переживает удаление автора
// (ON DELETE SET NULL) — общий пресет показывается как созданный удалённым
// пользователем, а не исчезает и не меняет владельца.
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

// Shared — true для общего фильтра проекта (без личного владельца).
func (f Filter) Shared() bool { return f.OwnerUserID == nil }

// ValidationError — отказ проверки фильтра с машинным кодом причины.
// По образцу internal/uptime/validation.go: доменный код, который веб-слой
// раскладывает в ключ "error.logfilter.<code>" — сам домен текста для
// интерфейса не собирает.
type ValidationError struct {
	Code  string
	Field string
}

func (e *ValidationError) Error() string { return "logfilter: " + e.Field + ": " + e.Code }

// payload — версионируемая форма хранения условий фильтра в jsonb.
// V != payloadVersion при чтении означает «формат неизвестен» — фильтр
// возвращается с Applicable=false и пустыми Predicates, а не роняет чтение.
type payload struct {
	V          int             `json:"v"`
	Predicates []log.Predicate `json:"predicates"`
}

// validateName проверяет имя фильтра: непустое после обрезки пробелов,
// не длиннее maxNameLen символов.
func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return &ValidationError{Code: "name_required", Field: "name"}
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return &ValidationError{Code: "name_too_long", Field: "name"}
	}
	return nil
}

// validatePredicates проверяет каждое условие по отдельности. Потолок числа
// условий сюда НЕ входит — validatePredicateCount вызывается отдельно,
// ПОСЛЕ log.NormalizePredicates (см. Store.Create/Update): здесь, до
// схлопывания дублей, count ещё может быть завышен кликами по одному и тому
// же условию.
func validatePredicates(preds []log.Predicate) error {
	for _, p := range preds {
		if err := p.Validate(); err != nil {
			return &ValidationError{Code: "invalid_predicate", Field: "predicates"}
		}
	}
	return nil
}

// validatePredicateCount проверяет потолок числа условий в ОДНОМ фильтре.
// Вызывается ПОСЛЕ log.NormalizePredicates (находка финального ревью C6):
// до фикса лимит считался ДО нормализации, и двадцать один одинаковый клик
// «исключить» давал ErrLimitReached/too_many_predicates там, где после
// схлопывания дублей реально остаётся одно условие.
func validatePredicateCount(preds []log.Predicate) error {
	if len(preds) > maxPredicates {
		return &ValidationError{Code: "too_many_predicates", Field: "predicates"}
	}
	return nil
}
