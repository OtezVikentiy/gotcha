package metric

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidRule = errors.New("metric: invalid alert rule")

var validAggregations = map[string]bool{
	"avg": true, "max": true, "min": true, "sum": true, "p50": true, "p95": true, "p99": true,
}

// Rule — правило порогового алерта на метрику.
type Rule struct {
	ID            int64
	ProjectID     int64
	MetricName    string
	Aggregation   string
	Comparator    string // 'gt' | 'lt'
	Threshold     float64
	WindowSeconds int
	Environment   string // "" → любой
	LabelKey      string // "" → без матчера
	LabelValue    string
	Enabled       bool
	CreatedAt     time.Time
}

const ruleColumns = `id, project_id, metric_name, aggregation, comparator, threshold,
	window_seconds, COALESCE(environment,''), COALESCE(label_key,''), COALESCE(label_value,''),
	enabled, created_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.ProjectID, &r.MetricName, &r.Aggregation, &r.Comparator, &r.Threshold,
		&r.WindowSeconds, &r.Environment, &r.LabelKey, &r.LabelValue, &r.Enabled, &r.CreatedAt)
	return r, err
}

// RuleService — CRUD правил (metric_alert_rules).
type RuleService struct {
	pool *pgxpool.Pool
}

func NewRuleService(pool *pgxpool.Pool) *RuleService {
	return &RuleService{pool: pool}
}

// Create валидирует и создаёт правило.
func (s *RuleService) Create(ctx context.Context, r Rule) (Rule, error) {
	if r.MetricName == "" || !validAggregations[r.Aggregation] ||
		(r.Comparator != "gt" && r.Comparator != "lt") || r.WindowSeconds <= 0 {
		return Rule{}, ErrInvalidRule
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO metric_alert_rules
			(project_id, metric_name, aggregation, comparator, threshold, window_seconds, environment, label_key, label_value, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10)
		RETURNING `+ruleColumns,
		r.ProjectID, r.MetricName, r.Aggregation, r.Comparator, r.Threshold, r.WindowSeconds,
		r.Environment, r.LabelKey, r.LabelValue, r.Enabled)
	out, err := scanRule(row)
	if err != nil {
		return Rule{}, fmt.Errorf("metric: create rule: %w", err)
	}
	return out, nil
}

// List возвращает правила проекта, свежайшие первыми.
func (s *RuleService) List(ctx context.Context, projectID int64) ([]Rule, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+ruleColumns+" FROM metric_alert_rules WHERE project_id = $1 ORDER BY created_at DESC", projectID)
	if err != nil {
		return nil, fmt.Errorf("metric: list rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("metric: list rules scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListEnabled возвращает все включённые правила (по всем проектам) — для оценщика.
func (s *RuleService) ListEnabled(ctx context.Context) ([]Rule, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+ruleColumns+" FROM metric_alert_rules WHERE enabled ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("metric: list enabled rules: %w", err)
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("metric: list enabled rules scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete удаляет правило проекта (scoped по projectID — чужое правило не удалить).
func (s *RuleService) Delete(ctx context.Context, id, projectID int64) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM metric_alert_rules WHERE id = $1 AND project_id = $2", id, projectID)
	if err != nil {
		return fmt.Errorf("metric: delete rule: %w", err)
	}
	return nil
}
