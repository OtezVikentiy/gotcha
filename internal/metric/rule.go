package metric

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidRule = errors.New("metric: invalid alert rule")

// Отличаем от ErrInvalidRule, чтобы web-слой отвечал 404, а не 422 с полями формы.
var ErrRuleNotFound = errors.New("metric: alert rule not found")

// Источник для сторожа динамических ключей i18n; validAggregations строится
// из него же, чтобы наборы не разъехались.
var Aggregations = []string{"avg", "max", "min", "sum", "increase", "p50", "p95", "p99"}

var validAggregations = func() map[string]bool {
	m := make(map[string]bool, len(Aggregations))
	for _, a := range Aggregations {
		m[a] = true
	}
	return m
}()

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
	Severity      string // "" → нет override (дефолт источника 'warning')
	CreatedAt     time.Time
}

const ruleColumns = `id, project_id, metric_name, aggregation, comparator, threshold,
	window_seconds, COALESCE(environment,''), COALESCE(label_key,''), COALESCE(label_value,''),
	enabled, COALESCE(severity,''), created_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.ProjectID, &r.MetricName, &r.Aggregation, &r.Comparator, &r.Threshold,
		&r.WindowSeconds, &r.Environment, &r.LabelKey, &r.LabelValue, &r.Enabled, &r.Severity, &r.CreatedAt)
	return r, err
}

type RuleService struct {
	pool *pgxpool.Pool
}

func NewRuleService(pool *pgxpool.Pool) *RuleService {
	return &RuleService{pool: pool}
}

// Общая для Create и Update: наборы условий не должны разъехаться между путями.
func validateRule(r Rule) error {
	if r.MetricName == "" || !validAggregations[r.Aggregation] ||
		(r.Comparator != "gt" && r.Comparator != "lt") || r.WindowSeconds <= 0 ||
		math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
		return ErrInvalidRule
	}
	return nil
}

func (s *RuleService) Create(ctx context.Context, r Rule) (Rule, error) {
	if err := validateRule(r); err != nil {
		return Rule{}, err
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO metric_alert_rules
			(project_id, metric_name, aggregation, comparator, threshold, window_seconds, environment, label_key, label_value, enabled, severity)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,NULLIF($11,''))
		RETURNING `+ruleColumns,
		r.ProjectID, r.MetricName, r.Aggregation, r.Comparator, r.Threshold, r.WindowSeconds,
		r.Environment, r.LabelKey, r.LabelValue, r.Enabled, r.Severity)
	out, err := scanRule(row)
	if err != nil {
		return Rule{}, fmt.Errorf("metric: create rule: %w", err)
	}
	return out, nil
}

// Сохранение с Enabled=false закрывает открытый инцидент правила в той же
// транзакции — иначе он завис бы навсегда, а эскалация слала бы по нему ступени.
func (s *RuleService) Update(ctx context.Context, r Rule) (Rule, error) {
	if err := validateRule(r); err != nil {
		return Rule{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Rule{}, fmt.Errorf("metric: update rule: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)
	var lockedID int64
	err = tx.QueryRow(ctx,
		"SELECT id FROM metric_alert_rules WHERE id = $1 AND project_id = $2 FOR UPDATE",
		r.ID, r.ProjectID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, fmt.Errorf("%w: rule %d in project %d", ErrRuleNotFound, r.ID, r.ProjectID)
	}
	if err != nil {
		return Rule{}, fmt.Errorf("metric: update rule: lock: %w", err)
	}
	row := tx.QueryRow(ctx, `
		UPDATE metric_alert_rules SET
			metric_name = $3, aggregation = $4, comparator = $5, threshold = $6,
			window_seconds = $7, environment = NULLIF($8,''), label_key = NULLIF($9,''),
			label_value = NULLIF($10,''), enabled = $11, severity = NULLIF($12,'')
		WHERE id = $1 AND project_id = $2
		RETURNING `+ruleColumns,
		r.ID, r.ProjectID, r.MetricName, r.Aggregation, r.Comparator, r.Threshold,
		r.WindowSeconds, r.Environment, r.LabelKey, r.LabelValue, r.Enabled, r.Severity)
	out, err := scanRule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, fmt.Errorf("%w: rule %d in project %d", ErrRuleNotFound, r.ID, r.ProjectID)
	}
	if err != nil {
		return Rule{}, fmt.Errorf("metric: update rule: %w", err)
	}
	if !out.Enabled {
		if err := resolveOpenIncidentForRule(ctx, tx, out.ID); err != nil {
			return Rule{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Rule{}, fmt.Errorf("metric: update rule: commit: %w", err)
	}
	return out, nil
}

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

// Без скоупинга по проекту — по всем проектам сразу, для оценщика.
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

// Без скоупинга по проекту: вызывающий уже знает projectID через инцидент,
// на который ссылается правило.
func (s *RuleService) Get(ctx context.Context, id int64) (Rule, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+ruleColumns+" FROM metric_alert_rules WHERE id = $1", id)
	r, err := scanRule(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, false, nil
	}
	if err != nil {
		return Rule{}, false, fmt.Errorf("metric: get rule: %w", err)
	}
	return r, true, nil
}

// Scoped по projectID — чужое правило не удалить.
func (s *RuleService) Delete(ctx context.Context, id, projectID int64) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM metric_alert_rules WHERE id = $1 AND project_id = $2", id, projectID)
	if err != nil {
		return fmt.Errorf("metric: delete rule: %w", err)
	}
	return nil
}
