package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const exportRowLimit = 10000

const exportTimeout = 30 * time.Second

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
	Breadcrumbs    string            `json:"breadcrumbs"`
	Request        string            `json:"request"`
}

// Субъект хранится в колонке user_id и в тегах user.id/enduser.id/user.email/enduser.email
// (см. txSubjectConds).
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

// ПДн субъекта лежат только в attributes (OTel: user.id/enduser.id/user.email).
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

// Субъект в spans не хранится колонкой — адресуется через trace_id совпавших transactions
// (см. exportSpansByTraceIDs), поэтому здесь ни одного условия WHERE по субъекту нет.
type SpanRow struct {
	ProjectID       uint64    `json:"project_id"`
	TraceID         string    `json:"trace_id"`
	SpanID          string    `json:"span_id"`
	ParentSpanID    string    `json:"parent_span_id"`
	Transaction     string    `json:"transaction"`
	Op              string    `json:"op"`
	Description     string    `json:"description"`
	DescriptionHash uint64    `json:"description_hash"`
	Timestamp       time.Time `json:"timestamp"`
	DurationUS      uint32    `json:"duration_us"`
	Status          string    `json:"status"`
	Environment     string    `json:"environment"`
	Data            string    `json:"data"`
	Source          string    `json:"source"`
}

// body — free-form текст сообщения, не фильтруется и отдаётся как есть.
type LogRow struct {
	ProjectID      uint64            `json:"project_id"`
	Timestamp      time.Time         `json:"timestamp"`
	ObservedTS     time.Time         `json:"observed_ts"`
	Severity       string            `json:"severity"`
	SeverityNumber uint8             `json:"severity_number"`
	SeverityText   string            `json:"severity_text"`
	Body           string            `json:"body"`
	TraceID        string            `json:"trace_id"`
	SpanID         string            `json:"span_id"`
	LogAttributes  map[string]string `json:"log_attributes"`
	ResourceAttrs  map[string]string `json:"resource_attrs"`
	Service        string            `json:"service"`
	Environment    string            `json:"environment"`
}

// Returned/Total совпадают только когда выгрузка по этой таблице полна; Total считается
// тем же WHERE, что и сама выборка, но без LIMIT — отдельным count(), как в PurgeSubject.
type SubjectExportCounts struct {
	Returned int    `json:"returned"`
	Total    uint64 `json:"total"`
}

// Counts несёт запись по каждому имени из subjectTables независимо от того, дал ли
// субъект условие для этой таблицы — иначе состав ключей плавал бы от запроса к запросу.
type SubjectExport struct {
	Events       []EventRow                     `json:"events"`
	Transactions []TransactionRow               `json:"transactions"`
	Spans        []SpanRow                      `json:"spans"`
	MetricPoints []MetricPointRow               `json:"metric_points"`
	Logs         []LogRow                       `json:"logs"`
	Counts       map[string]SubjectExportCounts `json:"counts"`
	Truncated    bool                           `json:"truncated"`
}

func (out *SubjectExport) recordCount(table string, returned int, total uint64) {
	out.Counts[table] = SubjectExportCounts{Returned: returned, Total: total}
	if uint64(returned) < total {
		out.Truncated = true
	}
}

// rows.Err() проверяется ПОСЛЕ цикла, не только rows.Close() — иначе обрыв курсора
// вернулся бы «успешно» с усечённой выгрузкой; Total в Counts — честный признак усечения.
func (p *Purger) ExportSubject(ctx context.Context, projectID int64, sub Subject) (SubjectExport, error) {
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()

	out := SubjectExport{Counts: map[string]SubjectExportCounts{}}
	for _, table := range subjectTables {
		out.Counts[table] = SubjectExportCounts{}
	}

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

	where := "project_id = ? AND (" + strings.Join(conds, " OR ") + ")"
	total, err := p.countMatching(ctx, "events", where, args)
	if err != nil {
		return SubjectExport{}, err
	}
	eventsQ := `SELECT event_id, project_id, issue_id, timestamp, level, message,
		exception_type, exception_value, stacktrace, environment, release,
		server_name, sdk, user_id, user_ip, user_email, tags, contexts,
		breadcrumbs, request
		FROM events WHERE ` + where + `
		ORDER BY timestamp DESC LIMIT ? SETTINGS max_execution_time = 0`
	rows, err := p.conn.Query(ctx, eventsQ, append(append([]any{}, args...), exportRowLimit)...)
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
			&r.Breadcrumbs, &r.Request,
		); err != nil {
			_ = rows.Close()
			return SubjectExport{}, fmt.Errorf("telemetry: scan event row (project %d): %w", projectID, err)
		}
		r.EventID = id.String()
		out.Events = append(out.Events, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SubjectExport{}, fmt.Errorf("telemetry: export subject events rows (project %d): %w", projectID, err)
	}
	if err := rows.Close(); err != nil {
		return SubjectExport{}, fmt.Errorf("telemetry: export subject events close (project %d): %w", projectID, err)
	}
	out.recordCount("events", len(out.Events), total)

	// По email — иначе субъект не увидит свои транзакции, там email лежит только в тегах.
	if txConds, txArgs := txSubjectConds(sub); len(txConds) > 0 {
		txArgsBase := append([]any{projectID}, txArgs...)
		txWhere := "project_id = ? AND (" + strings.Join(txConds, " OR ") + ")"

		// ДО выборки транзакций: trace_id субъекта берутся отсюда же, что и в PurgeSubject,
		// и не ограничены exportRowLimit — иначе спаны потерялись бы за пределами лимита transactions.
		traceIDs, err := p.matchingTraceIDs(ctx, txWhere, txArgsBase)
		if err != nil {
			return SubjectExport{}, err
		}
		spans, spansTotal, err := p.exportSpansByTraceIDs(ctx, projectID, traceIDs)
		if err != nil {
			return SubjectExport{}, err
		}
		out.Spans = spans
		out.recordCount("spans", len(out.Spans), spansTotal)

		txTotal, err := p.countMatching(ctx, "transactions", txWhere, txArgsBase)
		if err != nil {
			return SubjectExport{}, err
		}
		txQ := `SELECT project_id, trace_id, span_id, transaction, op, timestamp,
			duration_us, status, environment, release, server_name, user_id, tags, source
			FROM transactions WHERE ` + txWhere + `
			ORDER BY timestamp DESC LIMIT ? SETTINGS max_execution_time = 0`
		txRows, err := p.conn.Query(ctx, txQ, append(append([]any{}, txArgsBase...), exportRowLimit)...)
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
		if err := txRows.Err(); err != nil {
			_ = txRows.Close()
			return SubjectExport{}, fmt.Errorf("telemetry: export subject transactions rows (project %d): %w", projectID, err)
		}
		if err := txRows.Close(); err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject transactions close (project %d): %w", projectID, err)
		}
		out.recordCount("transactions", len(out.Transactions), txTotal)
	}

	// IP в attributes не бывает, поэтому по IP-only субъекту эту выборку пропускаем.
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
		mpWhere := "project_id = ? AND (" + strings.Join(mpConds, " OR ") + ")"
		mpTotal, err := p.countMatching(ctx, "metric_points", mpWhere, mpArgs)
		if err != nil {
			return SubjectExport{}, err
		}
		mpQ := `SELECT project_id, name, type, service, environment, attributes, ts, value
			FROM metric_points WHERE ` + mpWhere + `
			ORDER BY ts DESC LIMIT ? SETTINGS max_execution_time = 0`
		mpRows, err := p.conn.Query(ctx, mpQ, append(append([]any{}, mpArgs...), exportRowLimit)...)
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
		if err := mpRows.Err(); err != nil {
			_ = mpRows.Close()
			return SubjectExport{}, fmt.Errorf("telemetry: export subject metric_points rows (project %d): %w", projectID, err)
		}
		if err := mpRows.Close(); err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject metric_points close (project %d): %w", projectID, err)
		}
		out.recordCount("metric_points", len(out.MetricPoints), mpTotal)
	}

	// IP в log_attributes не бывает, поэтому по IP-only субъекту эту выборку пропускаем.
	var logConds []string
	logArgs := []any{projectID}
	if sub.UserID != "" {
		logConds = append(logConds, "log_attributes['user.id'] = ?", "log_attributes['enduser.id'] = ?")
		logArgs = append(logArgs, sub.UserID, sub.UserID)
	}
	if sub.Email != "" {
		logConds = append(logConds, "log_attributes['user.email'] = ?", "log_attributes['enduser.email'] = ?")
		logArgs = append(logArgs, sub.Email, sub.Email)
	}
	if len(logConds) > 0 {
		logWhere := "project_id = ? AND (" + strings.Join(logConds, " OR ") + ")"
		logTotal, err := p.countMatching(ctx, "logs", logWhere, logArgs)
		if err != nil {
			return SubjectExport{}, err
		}
		logQ := `SELECT project_id, timestamp, observed_ts, severity, severity_number,
			severity_text, body, trace_id, span_id, log_attributes, resource_attrs,
			service, environment
			FROM logs WHERE ` + logWhere + `
			ORDER BY timestamp DESC LIMIT ? SETTINGS max_execution_time = 0`
		logRows, err := p.conn.Query(ctx, logQ, append(append([]any{}, logArgs...), exportRowLimit)...)
		if err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject logs (project %d): %w", projectID, err)
		}
		for logRows.Next() {
			var r LogRow
			if err := logRows.Scan(
				&r.ProjectID, &r.Timestamp, &r.ObservedTS, &r.Severity, &r.SeverityNumber,
				&r.SeverityText, &r.Body, &r.TraceID, &r.SpanID, &r.LogAttributes, &r.ResourceAttrs,
				&r.Service, &r.Environment,
			); err != nil {
				_ = logRows.Close()
				return SubjectExport{}, fmt.Errorf("telemetry: scan log row (project %d): %w", projectID, err)
			}
			out.Logs = append(out.Logs, r)
		}
		if err := logRows.Err(); err != nil {
			_ = logRows.Close()
			return SubjectExport{}, fmt.Errorf("telemetry: export subject logs rows (project %d): %w", projectID, err)
		}
		if err := logRows.Close(); err != nil {
			return SubjectExport{}, fmt.Errorf("telemetry: export subject logs close (project %d): %w", projectID, err)
		}
		out.recordCount("logs", len(out.Logs), logTotal)
	}

	return out, nil
}

// Батч по spanTraceIDBatch — размер bind-параметра IN(...), как у purgeSpansByTraceIDs.
// total считается по ВСЕМ батчам, строки — только до exportRowLimit.
func (p *Purger) exportSpansByTraceIDs(ctx context.Context, projectID int64, traceIDs []string) ([]SpanRow, uint64, error) {
	var (
		out   []SpanRow
		total uint64
	)
	for len(traceIDs) > 0 {
		batch := traceIDs
		if len(batch) > spanTraceIDBatch {
			batch = traceIDs[:spanTraceIDBatch]
		}
		traceIDs = traceIDs[len(batch):]

		const where = "project_id = ? AND trace_id IN (?)"
		args := []any{projectID, batch}
		n, err := p.countMatching(ctx, "spans", where, args)
		if err != nil {
			return nil, 0, err
		}
		total += n

		if len(out) >= exportRowLimit {
			continue
		}
		q := `SELECT project_id, trace_id, span_id, parent_span_id, transaction, op,
			description, description_hash, timestamp, duration_us, status, environment,
			data, source
			FROM spans WHERE ` + where + `
			ORDER BY timestamp DESC LIMIT ? SETTINGS max_execution_time = 0`
		rows, err := p.conn.Query(ctx, q, append(append([]any{}, args...), exportRowLimit-len(out))...)
		if err != nil {
			return nil, 0, fmt.Errorf("telemetry: export subject spans (project %d): %w", projectID, err)
		}
		for rows.Next() {
			var r SpanRow
			if err := rows.Scan(
				&r.ProjectID, &r.TraceID, &r.SpanID, &r.ParentSpanID, &r.Transaction, &r.Op,
				&r.Description, &r.DescriptionHash, &r.Timestamp, &r.DurationUS, &r.Status,
				&r.Environment, &r.Data, &r.Source,
			); err != nil {
				_ = rows.Close()
				return nil, 0, fmt.Errorf("telemetry: scan span row (project %d): %w", projectID, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, 0, fmt.Errorf("telemetry: export subject spans rows (project %d): %w", projectID, err)
		}
		if err := rows.Close(); err != nil {
			return nil, 0, fmt.Errorf("telemetry: export subject spans close (project %d): %w", projectID, err)
		}
	}
	return out, total, nil
}
