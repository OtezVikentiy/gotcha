package export

import (
	"time"
)

type Kind string

const (
	KindIssues Kind = "issues"
	KindEvents Kind = "events"
)

func ParseKind(s string) (Kind, bool) {
	switch Kind(s) {
	case KindIssues, KindEvents:
		return Kind(s), true
	}
	return "", false
}

type Format string

const (
	FormatCSV    Format = "csv"
	FormatJSON   Format = "json"
	FormatNDJSON Format = "ndjson"
)

func ParseFormat(s string) (Format, bool) {
	switch Format(s) {
	case FormatCSV, FormatJSON, FormatNDJSON:
		return Format(s), true
	}
	return "", false
}

func (f Format) Ext() string { return string(f) }

func (f Format) ContentType() string {
	switch f {
	case FormatCSV:
		return "text/csv; charset=utf-8"
	case FormatJSON:
		return "application/json"
	case FormatNDJSON:
		return "application/x-ndjson"
	}
	return "application/octet-stream"
}

type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusExpired Status = "expired"
)

func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusExpired
}

// Since/Until уже развёрнуты в абсолютные значения — исполнение позже даёт тот же файл.
// Sort хранится для UI, но на порядок строк не влияет — обход всегда last_seen DESC, id DESC.
type Params struct {
	Status      string    `json:"status,omitempty"`
	Level       string    `json:"level,omitempty"`
	Query       string    `json:"query,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Sort        string    `json:"sort,omitempty"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`
}

type Job struct {
	ID           int64
	ProjectID    int64
	CreatedBy    int64
	Kind         Kind
	Format       Format
	ScopeIssueID int64 // 0 — выгрузка не ограничена одной группой
	Params       Params
	IncludePII   bool
	Status       Status
	Attempts     int
	LastError    string
	// Ключ не проверен: веб-слой обязан сверить его с KnownFailureReasonKey
	// перед i18n.T(), иначе пользователь увидит сырой технический идентификатор.
	FailureReasonKey string
	RowsWritten      int64
	Bytes            int64
	Truncated        bool
	FileExt          string
	ClaimedAt        *time.Time
	CreatedAt        time.Time
	FinishedAt       *time.Time
	ExpiresAt        *time.Time
}
