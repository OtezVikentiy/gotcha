package metric

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

var ErrIncidentNotFound = errors.New("metric: incident not found")

// Открытый или закрытый инцидент пробоя порога (metric_incidents).
type Incident struct {
	ID             int64
	RuleID         int64
	ProjectID      int64
	Status         string
	PeakValue      float64
	CurrentValue   float64
	StartedAt      time.Time
	ResolvedAt     *time.Time
	InMaintenance  bool
	NotifiedOpen   bool
	NotifiedClose  bool
	AcknowledgedAt *time.Time
	AcknowledgedBy *int64
	Severity       string
}

const incidentColumns = `id, rule_id, project_id, status, peak_value, current_value,
	started_at, resolved_at, in_maintenance, notified_open, notified_close,
	acknowledged_at, acknowledged_by, severity`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.RuleID, &in.ProjectID, &in.Status, &in.PeakValue, &in.CurrentValue,
		&in.StartedAt, &in.ResolvedAt, &in.InMaintenance, &in.NotifiedOpen, &in.NotifiedClose,
		&in.AcknowledgedAt, &in.AcknowledgedBy, &in.Severity)
	return in, err
}

// Атомарные open/close инцидентов (калька RegressionService).
type IncidentService struct {
	pool *pgxpool.Pool
}

func NewIncidentService(pool *pgxpool.Pool) *IncidentService {
	return &IncidentService{pool: pool}
}

// inMaintenance фиксируется на инциденте на всё его время — гейт notify смотрит на него, не на состояние
// окна при закрытии. Гонко-безопасно через partial unique index (rule_id) WHERE status='open'.
func (s *IncidentService) Open(ctx context.Context, ruleID, projectID int64, current float64, inMaintenance bool, severity string) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value, in_maintenance, severity)
		VALUES ($1, $2, $3, $3, $4, COALESCE(NULLIF($5,''), 'warning'))
		ON CONFLICT (rule_id) WHERE status = 'open' DO NOTHING
		RETURNING `+incidentColumns,
		ruleID, projectID, current, inMaintenance, severity)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, ruleID)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("metric: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("metric: open incident: %w", err)
	}
	return in, true, nil
}

func (s *IncidentService) OpenFor(ctx context.Context, ruleID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+incidentColumns+" FROM metric_incidents WHERE rule_id = $1 AND status = 'open'", ruleID)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("metric: open incident for: %w", err)
	}
	return in, true, nil
}

// Нужен эскалации: планировщик и StepNotifier знают только incidentID, объект приходится перегружать заново.
func (s *IncidentService) GetByID(ctx context.Context, id int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+incidentColumns+" FROM metric_incidents WHERE id = $1", id)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("metric: get incident by id: %w", err)
	}
	return in, true, nil
}

// current_value/peak_value обновляются (peak вычисляет вызывающий — экстремум в сторону нарушения).
// Закрытый инцидент/нет такого → ErrIncidentNotFound.
func (s *IncidentService) Bump(ctx context.Context, id int64, current, peak float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE metric_incidents SET current_value = $2, peak_value = $3
		WHERE id = $1 AND status = 'open'`, id, current, peak)
	if err != nil {
		return fmt.Errorf("metric: bump incident: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

// Единственный UPDATE, которым инцидент закрывается — используется и Resolve, и resolveOpenIncidentForRule,
// чтобы поля закрытия не могли разъехаться между путями.
const resolveIncidentSQL = `
		UPDATE metric_incidents SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`

// ok=false, если открытого не было (идемпотентно).
func (s *IncidentService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, resolveIncidentSQL, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metric: resolve incident: %w", err)
	}
	return true, nil
}

// Закрывает открытый инцидент правила при его выключении тем же resolveIncidentSQL — уведомление о
// восстановлении не шлётся намеренно (правило выключил оператор). Вызывается только из транзакции RuleService.Update.
func resolveOpenIncidentForRule(ctx context.Context, tx pgx.Tx, ruleID int64) error {
	var id int64
	var current float64
	err := tx.QueryRow(ctx,
		"SELECT id, current_value FROM metric_incidents WHERE rule_id = $1 AND status = 'open'",
		ruleID).Scan(&id, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("metric: open incident of disabled rule: %w", err)
	}
	if err := tx.QueryRow(ctx, resolveIncidentSQL, id, current).Scan(&id); err != nil {
		return fmt.Errorf("metric: resolve incident of disabled rule: %w", err)
	}
	return nil
}

// open → notified_open, иначе notified_close.
func (s *IncidentService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE metric_incidents SET "+column+" = true WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("metric: mark incident notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

// Фиксирует acknowledged_at/acknowledged_by, гасит дальнейшую эскалацию. ok=false, если уже подтверждён
// или закрыт (идемпотентно). project_id в WHERE — defense-in-depth, зеркало uptime.DeleteWindow.
func (s *IncidentService) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE metric_incidents SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND project_id = $2 AND status = 'open' AND acknowledged_at IS NULL
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metric: acknowledge incident: %w", err)
	}
	return true, nil
}

// Совпадает с incident_source='metric' в incident_escalations.
func (s *IncidentService) Name() string { return "metric" }

// Кандидаты планировщика эскалации. Члены ОТКРЫТЫХ групп исключены — информирование берёт на себя корень;
// у бывшего члена закрытой группы StartedAt=GREATEST(started_at, g.resolved_at) — иначе он получил бы всю лесенку разом.
func (s *IncidentService) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.project_id,
		       GREATEST(i.started_at, COALESCE(g.resolved_at, i.started_at)) AS started_at,
		       i.severity, i.escalation_level
		FROM metric_incidents i
		LEFT JOIN incident_groups g ON g.id = i.group_id
		WHERE i.status = 'open' AND i.acknowledged_at IS NULL
		  AND (i.group_id IS NULL OR g.id IS NULL OR g.resolved_at IS NOT NULL)
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("metric: open unacked incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("metric: open unacked incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Продвигает уровень эскалации с from на from+1, фиксирует last_escalated_at. ok=false, если level уже
// не равен from — планировщик проиграл гонку другому тику (идемпотентно).
func (s *IncidentService) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE metric_incidents SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("metric: bump escalation: %w", err)
	}
	return true, nil
}

func (s *IncidentService) List(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM metric_incidents WHERE project_id = $1 ORDER BY started_at DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("metric: list incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("metric: list incidents scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
