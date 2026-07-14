// Package event — поток событий: доменный тип и батч-запись в ClickHouse.
package event

import "time"

// Event — одно событие ошибки; поля соответствуют колонкам CH-таблицы events.
type Event struct {
	ID             string // canonical UUID
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
}
