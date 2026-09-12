package trace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

const (
	DecisionOpen    = "open"    // порог пробит, открытого инцидента нет → открыть
	DecisionResolve = "resolve" // метрика восстановилась → закрыть открытый
	DecisionNone    = "none"    // ничего не делать (нет базы/статистики/в норме)
)

// Value всегда в мс (CLS безразмерный) — Decide сам не конвертирует единицы,
// полагается на msSample/valueSample выше по стеку.
type RegressionSample struct {
	Value   float64
	Samples int
}

type Decision struct {
	Kind string // DecisionOpen | DecisionResolve | DecisionNone
}

// чистая функция — без БД/IO/времени; порядок проверок важен: сначала
// статистика/база, потом гистерезис закрытия, потом двойное условие открытия.
func Decide(base, recent RegressionSample, cfg RegressionConfig, metric string, open bool) Decision {
	if recent.Samples < cfg.MinSamples || base.Samples < cfg.MinSamples {
		return Decision{Kind: DecisionNone}
	}
	if base.Value <= 0 {
		return Decision{Kind: DecisionNone}
	}

	if open {
		// Гистерезис: закрываем, только когда вернулись под recovery-порог
		// (recovery_pct < threshold_pct — иначе инцидент мигал бы на границе).
		if recent.Value <= base.Value*(1+cfg.RecoveryPct) {
			return Decision{Kind: DecisionResolve}
		}
		return Decision{Kind: DecisionNone}
	}

	// Открытие только при одновременном выполнении относительного порога И
	// абсолютного пола: пол режет ложную тревогу на «+100% с 20 на 40 мс».
	if recent.Value > base.Value*(1+cfg.ThresholdPct) && recent.Value > base.Value+cfg.Floor(metric) {
		return Decision{Kind: DecisionOpen}
	}
	return Decision{Kind: DecisionNone}
}

// не более одного открытого инцидента на (project_id, target, metric) — держит
// perf_regressions_one_open_idx.
type Regression struct {
	ID         int64
	ProjectID  int64
	TargetKind string // 'endpoint_p95' | 'webvital_p75'
	Target     string
	Metric     string // 'duration' | 'lcp' | 'inp' | 'cls' | 'fcp' | 'ttfb'
	Status     string // 'open' | 'resolved'

	BaselineValue float64
	PeakValue     float64
	CurrentValue  float64

	StartedAt  time.Time
	ResolvedAt *time.Time

	InMaintenance  bool
	NotifiedOpen   bool
	NotifiedClose  bool
	AcknowledgedAt *time.Time
	AcknowledgedBy *int64
	Severity       string
}

const regressionColumns = `id, project_id, target_kind, target, metric, status, baseline_value, peak_value, current_value, started_at, resolved_at, in_maintenance, notified_open, notified_close, acknowledged_at, acknowledged_by, severity`

func scanRegression(row pgx.Row) (Regression, error) {
	var r Regression
	if err := row.Scan(&r.ID, &r.ProjectID, &r.TargetKind, &r.Target, &r.Metric, &r.Status,
		&r.BaselineValue, &r.PeakValue, &r.CurrentValue, &r.StartedAt, &r.ResolvedAt,
		&r.InMaintenance, &r.NotifiedOpen, &r.NotifiedClose,
		&r.AcknowledgedAt, &r.AcknowledgedBy, &r.Severity); err != nil {
		return Regression{}, err
	}
	return r, nil
}

type RegressionService struct {
	pool *pgxpool.Pool
}

func NewRegressionService(pool *pgxpool.Pool) *RegressionService {
	return &RegressionService{pool: pool}
}

// уникальный индекс — arbiter: один INSERT проходит, второй ловит DO NOTHING и дочитывает победителя.
// inMaintenance фиксируется на инциденте при открытии на всё его время.
func (s *RegressionService) Open(ctx context.Context, projectID int64, targetKind, target, metric string, base, current float64, inMaintenance bool) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO perf_regressions (project_id, target_kind, target, metric, baseline_value, peak_value, current_value, in_maintenance)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
		ON CONFLICT (project_id, target, metric) WHERE status = 'open' DO NOTHING
		RETURNING `+regressionColumns,
		projectID, targetKind, target, metric, base, current, inMaintenance)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, projectID, target, metric)
		if err != nil {
			return Regression{}, false, err
		}
		if !found {
			return Regression{}, false, fmt.Errorf("trace: open regression: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("trace: open regression: %w", err)
	}
	return r, true, nil
}

func (s *RegressionService) OpenFor(ctx context.Context, projectID int64, target, metric string) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+regressionColumns+`
		FROM perf_regressions WHERE project_id = $1 AND target = $2 AND metric = $3 AND status = 'open'`,
		projectID, target, metric)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Regression{}, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("trace: open regression for: %w", err)
	}
	return r, true, nil
}

// не переиспользует query.VitalKey (Transaction/Metric) — тот ключ живёт на
// стороне CH-агрегатов, этот на стороне хранилища регрессий в PG.
type RegressionKey struct {
	Target string
	Metric string
}

// отсутствие ключа в карте — это и есть «не открыт» (comma-ok); карта не
// предзаполняется нулями на входной список целей.
func (s *RegressionService) OpenForProject(ctx context.Context, projectID int64) (map[RegressionKey]Regression, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+regressionColumns+`
		FROM perf_regressions WHERE project_id = $1 AND status = 'open'`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("trace: open regressions for project: %w", err)
	}
	defer rows.Close()
	out := make(map[RegressionKey]Regression)
	for rows.Next() {
		r, err := scanRegression(rows)
		if err != nil {
			return nil, fmt.Errorf("trace: open regressions for project: %w", err)
		}
		out[RegressionKey{Target: r.Target, Metric: r.Metric}] = r
	}
	return out, rows.Err()
}

func (s *RegressionService) GetByID(ctx context.Context, id int64) (Regression, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+regressionColumns+" FROM perf_regressions WHERE id = $1", id)
	r, err := scanRegression(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Regression{}, false, nil
	}
	if err != nil {
		return Regression{}, false, fmt.Errorf("trace: get regression by id: %w", err)
	}
	return r, true, nil
}

func (s *RegressionService) Bump(ctx context.Context, id int64, current float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE perf_regressions
		SET current_value = $2, peak_value = GREATEST(peak_value, $2)
		WHERE id = $1 AND status = 'open'`, id, current)
	if err != nil {
		return fmt.Errorf("trace: bump regression: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ok=false, если открытого не было — повторный вызов после закрытия
// идемпотентен, не ошибка.
func (s *RegressionService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE perf_regressions
		SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("trace: resolve regression: %w", err)
	}
	return true, nil
}

func (s *RegressionService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE perf_regressions SET "+column+" = true WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("trace: mark regression notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ok=false, если уже подтверждён/закрыт (идемпотентно); project_id в WHERE —
// defense-in-depth, а не обязательное сужение (id и так уникален).
func (s *RegressionService) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE perf_regressions SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND project_id = $2 AND status = 'open' AND acknowledged_at IS NULL
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("trace: acknowledge regression: %w", err)
	}
	return true, nil
}

// должно совпадать с incident_source='trace' в incident_escalations.
func (s *RegressionService) Name() string { return "trace" }

func (s *RegressionService) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, started_at, severity, escalation_level
		FROM perf_regressions WHERE status = 'open' AND acknowledged_at IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("trace: open unacked regressions: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("trace: open unacked regressions scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CAS по escalation_level: ok=false — с ним параллельно сыграл другой тик
// планировщика (идемпотентно).
func (s *RegressionService) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE perf_regressions SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("trace: bump escalation: %w", err)
	}
	return true, nil
}

func (s *RegressionService) List(ctx context.Context, projectID int64, limit int) ([]Regression, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+regressionColumns+`
		FROM perf_regressions WHERE project_id = $1
		ORDER BY started_at DESC
		LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("trace: list regressions: %w", err)
	}
	defer rows.Close()
	var out []Regression
	for rows.Next() {
		r, err := scanRegression(rows)
		if err != nil {
			return nil, fmt.Errorf("trace: list regressions: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
