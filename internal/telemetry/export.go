package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// exportRowLimit ограничивает выгрузку на одну таблицу: право субъекта на доступ
// (152-ФЗ) не требует отдавать неограниченный объём — берём последние строки.
const exportRowLimit = 10000

// exportTimeout ограничивает суммарное время выгрузки: один большой экспорт не
// должен висеть на ClickHouse бесконечно (защита от Low-DoS).
const exportTimeout = 30 * time.Second

// EventRow — строка events, относящаяся к субъекту, пригодная для JSON-выгрузки.
// Перечислены все хранимые колонки: экспорт отдаёт ровно то, что реально лежит
// в ClickHouse по этому субъекту.
type EventRow struct {
	EventID        string            `json:"event_id"`
	ProjectID      uint64            `json:"project_id"`
	IssueID        uint64            `json:"issue_id"`
	Timestamp      time.Time         `json:"timestamp"`
	Level          string            `json:"level"`
	Message        string            `json:"message"`
	ExceptionType  string            `json:"exception_type"`
	ExceptionValue string            `json:"exception_value"`
	Stacktrace     string            `json:"stacktrace"`
	Environment    string            `json:"environment"`
	Release        string            `json:"release"`
	ServerName     string            `json:"server_name"`
	SDK            string            `json:"sdk"`
	UserID         string            `json:"user_id"`
	UserIP         string            `json:"user_ip"`
	UserEmail      string            `json:"user_email"`
	Tags           map[string]string `json:"tags"`
	Contexts       string            `json:"contexts"`
}

// TransactionRow — строка transactions субъекта. Из субъектных ПДн transactions
// хранят только user_id, остальные колонки отдаём для полноты выгрузки.
type TransactionRow struct {
	ProjectID   uint64            `json:"project_id"`
	TraceID     string            `json:"trace_id"`
	SpanID      string            `json:"span_id"`
	Transaction string            `json:"transaction"`
	Op          string            `json:"op"`
	Timestamp   time.Time         `json:"timestamp"`
	DurationUS  uint32            `json:"duration_us"`
	Status      string            `json:"status"`
	Environment string            `json:"environment"`
	Release     string            `json:"release"`
	ServerName  string            `json:"server_name"`
	UserID      string            `json:"user_id"`
	Tags        map[string]string `json:"tags"`
	Source      string            `json:"source"`
}

// MetricPointRow — точка метрики субъекта. ПДн субъекта в metric_points лежат
// только в attributes (OTel-конвенции user.id/enduser.id/user.email); остальные
// колонки отдаём для полноты выгрузки. JSON-сериализуема.
type MetricPointRow struct {
	ProjectID   uint64            `json:"project_id"`
	Name        string            `json:"name"`
	Type        string            `json:"type"`
	Service     string            `json:"service"`
	Environment string            `json:"environment"`
	Attributes  map[string]string `json:"attributes"`
	TS          time.Time         `json:"ts"`
	Value       float64           `json:"value"`
}

// SubjectExport — выгрузка всех ПДн субъекта в рамках проекта. Сериализуется в
// JSON для отдачи по праву субъекта на доступ (152-ФЗ, ст. 14).
type SubjectExport struct {
	Events       []EventRow       `json:"events"`
	Transactions []TransactionRow `json:"transactions"`
	MetricPoints []MetricPointRow `json:"metric_points"`
}

// ExportSubject возвращает всё, что хранится о субъекте в рамках проекта: строки
// events (по непустым user_email/user_id/user_ip), transactions (по user_id) и
// metric_points (по attributes user.id/enduser.id/user.email) — тот же охват, что
// чистит PurgeSubject, чтобы право на доступ было паритетно праву на удаление.
// Имена таблиц и колонок фиксированы; значения субъекта — только bound-параметры,
// инъекция невозможна. На таблицу отдаётся не более exportRowLimit строк,
// отсортированных по времени DESC (сначала свежие). Вся выгрузка ограничена
// exportTimeout.
func (p *Purger) ExportSubject(ctx context.Context, projectID int64, sub Subject) (SubjectExport, error) {
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()

	var out SubjectExport

	// events: OR по всем непустым идентификаторам субъекта.
	var conds []string
	args := []any{projectID}
	if sub.Email != "" {
		conds = append(conds, "user_email = ?")
		args = append(args, sub.Email)
	}
	if sub.UserID != "" {
		conds = append(conds, "user_id = ?")
		args = append(args, sub.UserID)
	}
	if sub.IP != "" {
		conds = append(conds, "user_ip = ?")
		args = append(args, sub.IP)
	}
	if len(conds) == 0 {
		return SubjectExport{}, fmt.Errorf("telemetry: export subject: empty subject")
	}

	eventsQ := `SELECT event_id, project_id, issue_id, timestamp, level, message,
		exception_type, exception_value, stacktrace, environment, release,
		server_name, sdk, user_id, user_ip, user_email, tags, contexts
		FROM events WHERE project_id = ? AND (` + strings.Join(conds, " OR ") + `)
		ORDER BY timestamp DESC LIMIT ?`
	args = append(args, exportRowLimit)

	rows, err := p.conn.Query(ctx, eventsQ, args...)
	if err != nil {
		return SubjectExport{}, fmt.Errorf("telemetry: export subject events (project %d): %w", projectID, err)
	}
	for rows.Next() {
		var r EventRow
		var id uuid.UUID
		if err := rows.Scan(
			&id, &r.ProjectID, &r.IssueID, &r.Timestamp, &r.Level, &r.Message,
			&r.ExceptionType, &r.ExceptionValue, &r.Stacktrace, &r.Environment, &r.Release,
			&r.ServerName, &r.SDK, &r.UserID, &r.UserIP, &r.UserEmail, &r.Tags, &r.Contexts,
		); err != nil {
			_ = rows.Close()
			return SubjectExport{}, fmt.Errorf("telemetry: scan event row (project %d): %w", projectID, err)
		}
		r.EventID = id.String()
		out.Events = append(out.Events, r)
	}
	if err := rows.Close(); err != nil {
		return SubjectExport{}, fmt.Errorf("telemetry: export subject events close (project %d): %w", projectID, err)
	}

	// transactions хранят из субъектных ПДн только user_id.
	if sub.UserID != "" {
		txQ := `SELECT project_id, trace_id, span_id, transaction, op, timestamp,
			duration_us, status, environment, release, server_name, user_id, tags, source
			FROM transactions WHERE project_id = ? AND user_id = ?
			ORDER BY timestamp DESC LIMIT ?`
		txRows, err := p.conn.Query(ctx, txQ, projectID, sub.UserID, exportRowLimit)
		if err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject transactions (project %d): %w", projectID, err)
		}
		for txRows.Next() {
			var r TransactionRow
			if err := txRows.Scan(
				&r.ProjectID, &r.TraceID, &r.SpanID, &r.Transaction, &r.Op, &r.Timestamp,
				&r.DurationUS, &r.Status, &r.Environment, &r.Release, &r.ServerName, &r.UserID, &r.Tags, &r.Source,
			); err != nil {
				_ = txRows.Close()
				return SubjectExport{}, fmt.Errorf("telemetry: scan transaction row (project %d): %w", projectID, err)
			}
			out.Transactions = append(out.Transactions, r)
		}
		if err := txRows.Close(); err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject transactions close (project %d): %w", projectID, err)
		}
	}

	// metric_points несут ПДн субъекта только в attributes (Map(String,String)):
	// user.id/enduser.id ← UserID, user.email ← Email. IP в attributes не бывает,
	// поэтому по IP-only субъекту эту выборку пропускаем (как и PurgeSubject).
	var mpConds []string
	mpArgs := []any{projectID}
	if sub.UserID != "" {
		mpConds = append(mpConds, "attributes['user.id'] = ?", "attributes['enduser.id'] = ?")
		mpArgs = append(mpArgs, sub.UserID, sub.UserID)
	}
	if sub.Email != "" {
		mpConds = append(mpConds, "attributes['user.email'] = ?")
		mpArgs = append(mpArgs, sub.Email)
	}
	if len(mpConds) > 0 {
		mpQ := `SELECT project_id, name, type, service, environment, attributes, ts, value
			FROM metric_points WHERE project_id = ? AND (` + strings.Join(mpConds, " OR ") + `)
			ORDER BY ts DESC LIMIT ?`
		mpArgs = append(mpArgs, exportRowLimit)
		mpRows, err := p.conn.Query(ctx, mpQ, mpArgs...)
		if err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject metric_points (project %d): %w", projectID, err)
		}
		for mpRows.Next() {
			var r MetricPointRow
			if err := mpRows.Scan(
				&r.ProjectID, &r.Name, &r.Type, &r.Service, &r.Environment, &r.Attributes, &r.TS, &r.Value,
			); err != nil {
				_ = mpRows.Close()
				return SubjectExport{}, fmt.Errorf("telemetry: scan metric_point row (project %d): %w", projectID, err)
			}
			out.MetricPoints = append(out.MetricPoints, r)
		}
		if err := mpRows.Close(); err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject metric_points close (project %d): %w", projectID, err)
		}
	}

	return out, nil
}
