package event

import "time"

// Поля соответствуют колонкам CH-таблицы events.
type Event struct {
	ID string // canonical UUID
	// В CH-таблицу events не пишется (там proj-скоуп) — нужен для атрибуции
	// дропов буфера писателя в org_usage.dropped_*; 0 — атрибутировать некуда.
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
	// Из contexts.trace события; пустые, если SDK трейсинг не включил.
	TraceID string
	SpanID  string
}
