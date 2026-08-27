// Package export — фоновые выгрузки ошибок и событий: очередь заявок в PG,
// воркер собирает файл на диск, автор скачивает его со страницы выгрузок.
package export

import (
	"time"
)

// Kind — что выгружаем: группы ошибок или сырые события.
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

// Format — формат файла выгрузки.
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

// Ext — расширение файла на диске и в имени скачивания.
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

// Status — состояние заявки в очереди.
type Status string

const (
	StatusQueued  Status = "queued"
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusExpired Status = "expired"
)

// Terminal — заявка досчитана: файл больше не пишется, строку можно удалять.
func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusExpired
}

// Params — снимок фильтров на момент постановки в очередь. Относительный период
// уже развёрнут в абсолютные Since/Until: заявка «за последний час», исполненная
// через десять минут, обязана дать тот же файл, что дала бы сразу.
//
// Sort хранится как часть снимка UI (форма постановки заявки пишет его сюда,
// он же переносится в issue.Filter.Sort обоими источниками), но на порядок
// строк САМОЙ выгрузки не влияет: StreamForExport/IDsForFilter обходят
// результат жёстко по last_seen DESC, id DESC — иначе не построить
// keyset-курсор постранично устойчивого обхода. Поле не мёртвый код (уходит
// в params jsonb вместе с остальным снимком фильтра), просто его значение
// нигде не читается при сборке файла.
type Params struct {
	Status      string    `json:"status,omitempty"`
	Level       string    `json:"level,omitempty"`
	Query       string    `json:"query,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Sort        string    `json:"sort,omitempty"`
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`
}

// Job — заявка на выгрузку: строка таблицы export_jobs.
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
	// FailureReasonKey — ключ i18n.T() переведённой причины отказа: тот же
	// снимок несёт его и в письмо автору (mailPayload в notify.go), и в
	// колонку failure_reason_key export_jobs (P2-UX-2 аудита — раньше
	// колонки не было, и на инстансе без почты причина не попадала автору
	// никуда). Fail/FailPermanent/SweepStale (store.go) пишут в неё ТОЛЬКО
	// при переходе в status='failed' — заявка, вернувшаяся в очередь на
	// повтор, ключ не несёт (следующая попытка может завершиться иначе).
	// NULL/пусто у строки — заявка не терминальна либо старше этой колонки;
	// scanJob отдаёт такую строку пустым FailureReasonKey, а не паникует.
	// Значение из БД — до недоверия то же самое, что last_error (сырая
	// строка): веб-слой обязан сверить его с export.KnownFailureReasonKey
	// перед i18n.T(), а не подставлять напрямую (см. exportViewRow в
	// internal/web/exports.go) — i18n.T() на неизвестном ключе возвращает
	// сам ключ, и пользователь увидел бы технический идентификатор.
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
