// Package event — поток событий: доменный тип и батч-запись в ClickHouse.
package event

import "time"

// Event — одно событие ошибки; поля соответствуют колонкам CH-таблицы events.
type Event struct {
	ID string // canonical UUID
	// OrgID — организация проекта. В CH-таблицу events НЕ пишется (там proj-скоуп),
	// нужен только для per-org атрибуции дропов буфера писателя в org_usage.dropped_*
	// (см. Batcher.SetDropSink): при переполнении буфера выброшенные строки надо
	// списать той организации, которой они принадлежали. 0 — атрибутировать некуда.
	OrgID          int64
	ProjectID      int64
	IssueID        int64
	Timestamp      time.Time
	Level          string
	Message        string
	ExceptionType  string
	ExceptionValue string
	Stacktrace     string // JSON
	Environment    string
	Release        string
	ServerName     string
	SDK            string
	UserID         string
	UserIP         string
	UserEmail      string
	Tags           map[string]string
	Contexts       string // JSON
	Breadcrumbs    string // JSON (Sentry breadcrumbs.values)
	Request        string // JSON (Sentry request-интерфейс: method/url/query_string/data/headers)
	// TraceID/SpanID — из contexts.trace события: связывают ошибку с
	// транзакцией трейсинга (пустые, если SDK трейсинг не включил).
	TraceID string
	SpanID  string
}
