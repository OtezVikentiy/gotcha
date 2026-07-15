package profile

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRegressionNotFound = errors.New("profile: regression not found")

// Regression — строка profile_regressions (для оценщика и UI).
type Regression struct {
	ID            int64
	ProjectID     int64
	Service       string
	ProfileType   string
	Function      string
	Status        string
	BaselineShare float64
	PeakShare     float64
	CurrentShare  float64
	StartedAt     time.Time
	ResolvedAt    *time.Time
	NotifiedOpen  bool
	NotifiedClose bool
}

const regressionColumns = `id, project_id, service, profile_type, function, status,
	baseline_share, peak_share, current_share, started_at, resolved_at, notified_open, notified_close`

func scanRegression(row pgx.Row) (Regression, error) {
	var r Regression
	err := row.Scan(&r.ID, &r.ProjectID, &r.Service, &r.ProfileType, &r.Function, &r.Status,
		&r.BaselineShare, &r.PeakShare, &r.CurrentShare, &r.StartedAt, &r.ResolvedAt,
		&r.NotifiedOpen, &r.NotifiedClose)
	return r, err
}

// RegressionService — атомарные open/close инцидентов profile_regressions
// (калька trace.RegressionService / metric.IncidentService).
type RegressionService struct {
	pool *pgxpool.Pool
}

func NewRegressionService(pool *pgxpool.Pool) *RegressionService {
	return &RegressionService{pool: pool}
}

// Open открывает инцидент по (project,service,type,function), если открытого
// нет. Гонко-безопасно через partial-индекс profile_regressions_one_open_idx:
// из параллельных вызовов ровно один INSERT проходит, остальные ловят конфликт
// (DO NOTHING → нет RETURNING) и дочитывают победителя. peak=current на вставке.
func (s *RegressionService) Open(ctx context.Context, projectID int64, service, profileType, function string, base, current float64) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO profile_regressions (project_id, service, profile_type, function, baseline_share, peak_share, current_share)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		ON CONFLICT (project_id, service, profile_type, function) WHERE status = 'open' DO NOTHING
		RETURNING `+regressionColumns,
		projectID, service, profileType, function, base, current)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, projectID, service, profileType, function)
		if err != nil {
			return Regression{}, false, err
		}
		if !found {
			return Regression{}, false, fmt.Errorf("profile: open regression: conflicted but no open incident")
		}
		return existing, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("profile: open regression: %w", err)
	}
	return r, true, nil
}

// OpenFor возвращает открытый инцидент по ключу, если он есть.
func (s *RegressionService) OpenFor(ctx context.Context, projectID int64, service, profileType, function string) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+regressionColumns+` FROM profile_regressions
		 WHERE project_id=$1 AND service=$2 AND profile_type=$3 AND function=$4 AND status='open'`,
		projectID, service, profileType, function)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Regression{}, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("profile: open regression for: %w", err)
	}
	return r, true, nil
}

// Bump обновляет открытый инцидент: current_share=$2, peak_share=max(peak,$2).
func (s *RegressionService) Bump(ctx context.Context, id int64, current float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE profile_regressions SET current_share=$2, peak_share=GREATEST(peak_share,$2)
		WHERE id=$1 AND status='open'`, id, current)
	if err != nil {
		return fmt.Errorf("profile: bump regression: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRegressionNotFound
	}
	return nil
}

// Resolve закрывает открытый инцидент. ok=false, если открытого не было (идемпотентно).
func (s *RegressionService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE profile_regressions SET status='resolved', resolved_at=now(), current_share=$2
		WHERE id=$1 AND status='open'
		RETURNING id`, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("profile: resolve regression: %w", err)
	}
	return true, nil
}

// MarkNotified фиксирует отправку уведомления (open → notified_open, иначе notified_close).
func (s *RegressionService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE profile_regressions SET "+column+"=true WHERE id=$1", id)
	if err != nil {
		return fmt.Errorf("profile: mark regression notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRegressionNotFound
	}
	return nil
}

// List возвращает регрессии проекта, свежайшие первыми. status: ""/"all" — все,
// "open"/"resolved" — фильтр.
func (s *RegressionService) List(ctx context.Context, projectID int64, status string, limit int) ([]Regression, error) {
	if limit <= 0 {
		limit = 200
	}
	q := "SELECT " + regressionColumns + " FROM profile_regressions WHERE project_id=$1"
	args := []any{projectID}
	if status == "open" || status == "resolved" {
		q += " AND status=$2"
		args = append(args, status)
	}
	q += fmt.Sprintf(" ORDER BY started_at DESC LIMIT %d", limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("profile: list regressions: %w", err)
	}
	defer rows.Close()
	var out []Regression
	for rows.Next() {
		r, err := scanRegression(rows)
		if err != nil {
			return nil, fmt.Errorf("profile: list regressions scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
