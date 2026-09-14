package telemetry

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Имена только отсюда, никогда из пользовательского ввода — подставляются в запрос напрямую.
// transactions_5m/web_vitals_5m — материализованные представления, DELETE идёт по их таблице.
var projectTables = []string{
	"events",
	"transactions",
	"spans",
	"metric_points",
	"profile_samples",
	"check_results",
	"logs",
	"transactions_5m",
	"web_vitals_5m",
}

// Единый охват PurgeSubject и ExportSubject: расхождение состава означало бы, что
// субъекту стирают больше или меньше, чем ему показывают по запросу доступа.
var subjectTables = []string{
	"events",
	"transactions",
	"spans",
	"metric_points",
	"logs",
}

// CH-таблицы с прямой колонкой субъекта (user_id/user_email/user_ip). metric_points/logs
// несут субъекта только в Map-атрибутах, а spans — косвенно через trace_id, поэтому не входят.
var subjectColumnTables = []string{
	"events",
	"transactions",
}

type Subject struct {
	Email  string
	UserID string
	IP     string
}

// При GOTCHA_SCRUB_IP/GOTCHA_SCRUB_EMAIL email и IP зануляются на приёме, и поиск по ним
// не совпадёт никогда — только user_id. PurgeResult различает «удалено N» от «не найдено».
type PurgeResult struct {
	Events       uint64
	Transactions uint64
	Spans        uint64
	MetricPoints uint64
	Logs         uint64
}

func (r PurgeResult) Total() uint64 {
	return r.Events + r.Transactions + r.Spans + r.MetricPoints + r.Logs
}

type Purger struct {
	conn driver.Conn
}

func NewPurger(conn driver.Conn) *Purger {
	return &Purger{conn: conn}
}

// mutations_sync=2 делает ALTER ... DELETE синхронным; max_execution_time=0 снимает
// потолок в 60с — синхронная мутация по 90-дневным данным идёт минуты.
func (p *Purger) PurgeProject(ctx context.Context, projectID int64) error {
	for _, t := range projectTables {
		q := "ALTER TABLE " + t + " DELETE WHERE project_id = ? SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, q, projectID); err != nil {
			return fmt.Errorf("telemetry: purge project %d from %s: %w", projectID, t, err)
		}
	}
	return nil
}

// spans адресуются косвенно, через trace_id transactions, и удаляются ДО них: иначе
// сбой на spans после стёртых transactions лишит retry возможности найти trace_id.
func (p *Purger) PurgeSubject(ctx context.Context, projectID int64, sub Subject) (PurgeResult, error) {
	var res PurgeResult

	var conds []string
	var args []any
	args = append(args, projectID)
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
		return res, fmt.Errorf("telemetry: purge subject: empty subject")
	}

	where := "project_id = ? AND (" + strings.Join(conds, " OR ") + ")"
	n, err := p.countMatching(ctx, "events", where, args)
	if err != nil {
		return res, err
	}
	res.Events = n
	eventsQ := "ALTER TABLE events DELETE WHERE " + where +
		" SETTINGS mutations_sync = 2, max_execution_time = 0"
	if err := p.conn.Exec(ctx, eventsQ, args...); err != nil {
		return res, fmt.Errorf("telemetry: purge subject from events (project %d): %w", projectID, err)
	}

	if txConds, txArgs := txSubjectConds(sub); len(txConds) > 0 {
		args := append([]any{projectID}, txArgs...)
		txWhere := "project_id = ? AND (" + strings.Join(txConds, " OR ") + ")"

		traceIDs, err := p.matchingTraceIDs(ctx, txWhere, args)
		if err != nil {
			return res, err
		}

		spansDeleted, err := p.purgeSpansByTraceIDs(ctx, projectID, traceIDs)
		if err != nil {
			return res, err
		}
		res.Spans = spansDeleted

		n, err := p.countMatching(ctx, "transactions", txWhere, args)
		if err != nil {
			return res, err
		}
		res.Transactions = n
		txQ := "ALTER TABLE transactions DELETE WHERE " + txWhere +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, txQ, args...); err != nil {
			return res, fmt.Errorf("telemetry: purge subject from transactions (project %d): %w", projectID, err)
		}
	}

	// user_ip в attributes не встречается, поэтому в условие не входит.
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
		n, err := p.countMatching(ctx, "metric_points", mpWhere, mpArgs)
		if err != nil {
			return res, err
		}
		res.MetricPoints = n
		mpQ := "ALTER TABLE metric_points DELETE WHERE " + mpWhere +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, mpQ, mpArgs...); err != nil {
			return res, fmt.Errorf("telemetry: purge subject from metric_points (project %d): %w", projectID, err)
		}
	}

	// user_ip в log_attributes не встречается, поэтому в условие не входит.
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
		n, err := p.countMatching(ctx, "logs", logWhere, logArgs)
		if err != nil {
			return res, err
		}
		res.Logs = n
		logQ := "ALTER TABLE logs DELETE WHERE " + logWhere +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, logQ, logArgs...); err != nil {
			return res, fmt.Errorf("telemetry: purge subject from logs (project %d): %w", projectID, err)
		}
	}
	return res, nil
}

// Имя таблицы — только из литералов этого файла, все значения — связанные параметры.
func (p *Purger) countMatching(ctx context.Context, table, where string, args []any) (uint64, error) {
	var n uint64
	q := "SELECT count() FROM " + table + " WHERE " + where
	if err := p.conn.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("telemetry: count subject rows in %s: %w", table, err)
	}
	return n, nil
}

// Вызывается ДО удаления строк transactions — потом trace_id субъекта взять уже неоткуда.
func (p *Purger) matchingTraceIDs(ctx context.Context, where string, args []any) ([]string, error) {
	rows, err := p.conn.Query(ctx, "SELECT DISTINCT trace_id FROM transactions WHERE "+where, args...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: collect subject trace_id: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("telemetry: scan subject trace_id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("telemetry: collect subject trace_id: %w", err)
	}
	return ids, nil
}

// Ограничивает размер одного bind-параметра IN(...): субъект мог оставить много trace_id.
const spanTraceIDBatch = 5000

func (p *Purger) purgeSpansByTraceIDs(ctx context.Context, projectID int64, traceIDs []string) (uint64, error) {
	var total uint64
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
			return total, err
		}
		total += n

		q := "ALTER TABLE spans DELETE WHERE " + where +
			" SETTINGS mutations_sync = 2, max_execution_time = 0"
		if err := p.conn.Exec(ctx, q, args...); err != nil {
			return total, fmt.Errorf("telemetry: purge subject spans (project %d): %w", projectID, err)
		}
	}
	return total, nil
}

// IP в transactions не хранится, поэтому IP-only субъект не даёт условий и не затрагивает их.
func txSubjectConds(sub Subject) (conds []string, args []any) {
	if sub.UserID != "" {
		conds = append(conds, "user_id = ?", "tags['user.id'] = ?", "tags['enduser.id'] = ?")
		args = append(args, sub.UserID, sub.UserID, sub.UserID)
	}
	if sub.Email != "" {
		conds = append(conds, "tags['user.email'] = ?", "tags['enduser.email'] = ?")
		args = append(args, sub.Email, sub.Email)
	}
	return conds, args
}
