package metric

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrIncidentNotFound = errors.New("metric: incident not found")

// Incident — открытый или закрытый инцидент пробоя порога (metric_incidents).
type Incident struct {
	ID            int64
	RuleID        int64
	ProjectID     int64
	Status        string
	PeakValue     float64
	CurrentValue  float64
	StartedAt     time.Time
	ResolvedAt    *time.Time
	NotifiedOpen  bool
	NotifiedClose bool
}

const incidentColumns = `id, rule_id, project_id, status, peak_value, current_value,
	started_at, resolved_at, notified_open, notified_close`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.RuleID, &in.ProjectID, &in.Status, &in.PeakValue, &in.CurrentValue,
		&in.StartedAt, &in.ResolvedAt, &in.NotifiedOpen, &in.NotifiedClose)
	return in, err
}

// IncidentService — атомарные open/close инцидентов (калька RegressionService).
type IncidentService struct {
	pool *pgxpool.Pool
}

func NewIncidentService(pool *pgxpool.Pool) *IncidentService {
	return &IncidentService{pool: pool}
}

// Open открывает инцидент по правилу, если открытого ещё нет. Гонко-безопасно
// через частичный уникальный индекс metric_incidents_one_open_idx (rule_id)
// WHERE status='open': из параллельных вызовов ровно один INSERT проходит,
// остальные ловят конфликт (DO NOTHING → нет RETURNING) и дочитывают
// победителя. peak=current на вставке.
func (s *IncidentService) Open(ctx context.Context, ruleID, projectID int64, current float64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO metric_incidents (rule_id, project_id, peak_value, current_value)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (rule_id) WHERE status = 'open' DO NOTHING
		RETURNING `+incidentColumns,
		ruleID, projectID, current)
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

// OpenFor возвращает открытый инцидент правила, если он есть.
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

// Bump обновляет открытый инцидент: current_value=$2, peak_value=$3 (peak
// вычисляет вызывающий — экстремум в сторону нарушения). Закрытый/нет → ErrIncidentNotFound.
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

// Resolve закрывает открытый инцидент. ok=false, если открытого не было
// (идемпотентно).
func (s *IncidentService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE metric_incidents SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`, id, current)
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

// MarkNotified фиксирует отправку уведомления (open → notified_open, иначе
// notified_close).
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

// List возвращает инциденты проекта, свежайшие первыми (для UI).
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
