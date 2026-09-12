package slo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

// Капается по рунам в имени SLO, не по байтам.
const maxName = 200

// без кап-на-проект оператор плодит latency-SLO (raw-скан ~10с каждый), а
// последовательный оценщик растягивает тик за интервал — задержка бьёт по ВСЕМ тенантам.
const maxSLOsPerProject = 100

// экспортируется, чтобы web-слой отличил её от прочих ошибок и отдал 422, не 500.
var ErrTooManySLOs = errors.New("slo: too many slos for project")

// без неё «удалили ничего» неотличимо от успеха, и web-слой рапортует не туда.
var ErrNotFound = errors.New("slo: not found")

// не `cap`: шадовило бы builtin.
func capStr(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// все запросы скоупятся по project_id, кроме ListEnabled — тот читает все
// включённые SLO по всем проектам для оценщика.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const sloColumns = `id, project_id, name, sli_kind, target, window_days,
	transaction, environment, threshold_ms, monitor_id,
	burn_threshold, burn_long_minutes, burn_short_minutes,
	enabled, created_at, updated_at`

func scanSLO(row pgx.Row) (SLO, error) {
	var s SLO
	err := row.Scan(&s.ID, &s.ProjectID, &s.Name, &s.Kind, &s.Target, &s.WindowDays,
		&s.Transaction, &s.Environment, &s.ThresholdMS, &s.MonitorID,
		&s.BurnThreshold, &s.BurnLongMin, &s.BurnShortMin,
		&s.Enabled, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

func (s *Store) Create(ctx context.Context, in SLO) (SLO, error) {
	in.Name = capStr(in.Name, maxName)
	in.Transaction = capStr(in.Transaction, maxName)
	in.Environment = capStr(in.Environment, maxName)
	// при гонке на границе возможен небольшой перелёт — это ограничение
	// blast-radius, не защита безопасности, точность до единицы не нужна.
	var count int
	if err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM slos WHERE project_id = $1", in.ProjectID).Scan(&count); err != nil {
		return SLO{}, fmt.Errorf("slo: create count: %w", err)
	}
	if count >= maxSLOsPerProject {
		return SLO{}, ErrTooManySLOs
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO slos
			(project_id, name, sli_kind, target, window_days, transaction, environment,
			 threshold_ms, monitor_id, burn_threshold, burn_long_minutes, burn_short_minutes, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+sloColumns,
		in.ProjectID, in.Name, in.Kind, in.Target, in.WindowDays, in.Transaction, in.Environment,
		in.ThresholdMS, in.MonitorID, in.BurnThreshold, in.BurnLongMin, in.BurnShortMin, in.Enabled)
	out, err := scanSLO(row)
	if err != nil {
		return SLO{}, fmt.Errorf("slo: create: %w", err)
	}
	return out, nil
}

func (s *Store) Get(ctx context.Context, projectID, id int64) (SLO, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+sloColumns+" FROM slos WHERE project_id = $1 AND id = $2", projectID, id)
	out, err := scanSLO(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return SLO{}, false, nil
	}
	if err != nil {
		return SLO{}, false, fmt.Errorf("slo: get: %w", err)
	}
	return out, true, nil
}

func (s *Store) List(ctx context.Context, projectID int64) ([]SLO, error) {
	// запас над maxSLOsPerProject: даже если кап обойдён, список не станет O(N).
	rows, err := s.pool.Query(ctx,
		"SELECT "+sloColumns+" FROM slos WHERE project_id = $1 ORDER BY created_at DESC, id DESC LIMIT 200", projectID)
	if err != nil {
		return nil, fmt.Errorf("slo: list: %w", err)
	}
	defer rows.Close()
	var out []SLO
	for rows.Next() {
		one, err := scanSLO(rows)
		if err != nil {
			return nil, fmt.Errorf("slo: list scan: %w", err)
		}
		out = append(out, one)
	}
	return out, rows.Err()
}

func (s *Store) ListEnabled(ctx context.Context) ([]SLO, error) {
	// вторичная защита: кап-на-проект не ограничивает число проектов; 5000 SLO —
	// уже за гранью разумного для одного тика.
	rows, err := s.pool.Query(ctx,
		"SELECT "+sloColumns+" FROM slos WHERE enabled ORDER BY id LIMIT 5000")
	if err != nil {
		return nil, fmt.Errorf("slo: list enabled: %w", err)
	}
	defer rows.Close()
	var out []SLO
	for rows.Next() {
		one, err := scanSLO(rows)
		if err != nil {
			return nil, fmt.Errorf("slo: list enabled scan: %w", err)
		}
		out = append(out, one)
	}
	return out, rows.Err()
}

// инциденты уходят каскадом (ON DELETE CASCADE).
func (s *Store) Delete(ctx context.Context, projectID, id int64) error {
	tag, err := s.pool.Exec(ctx,
		"DELETE FROM slos WHERE project_id = $1 AND id = $2", projectID, id)
	if err != nil {
		return fmt.Errorf("slo: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const incidentColumns = `id, slo_id, project_id, status, burn_rate, budget_remaining,
	started_at, resolved_at, in_maintenance, notified_open, notified_close,
	acknowledged_at, acknowledged_by, severity`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.SLOID, &in.ProjectID, &in.Status, &in.BurnRate, &in.BudgetRemaining,
		&in.StartedAt, &in.ResolvedAt, &in.InMaintenance, &in.NotifiedOpen, &in.NotifiedClose,
		&in.AcknowledgedAt, &in.AcknowledgedBy, &in.Severity)
	return in, err
}

func (s *Store) OpenIncidentFor(ctx context.Context, sloID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+incidentColumns+" FROM slo_incidents WHERE slo_id = $1 AND status = 'open'", sloID)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("slo: open incident for: %w", err)
	}
	return in, true, nil
}

// не путать с Get — тот читает определение SLO (slos), а не инцидент.
func (s *Store) GetIncidentByID(ctx context.Context, id int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+incidentColumns+" FROM slo_incidents WHERE id = $1", id)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("slo: get incident by id: %w", err)
	}
	return in, true, nil
}

// one-open через частичный уникальный индекс: транзакция SELECT-then-INSERT,
// при гонке параллельный INSERT ловит уникальное нарушение — перечитываем победителя.
func (s *Store) OpenIncident(ctx context.Context, sloID, projectID int64, burnRate float64, budgetRemaining *float64, inMaintenance bool) (Incident, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Incident{}, false, fmt.Errorf("slo: open incident begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existing, err := scanIncident(tx.QueryRow(ctx,
		"SELECT "+incidentColumns+" FROM slo_incidents WHERE slo_id = $1 AND status = 'open'", sloID))
	if err == nil {
		if cErr := tx.Commit(ctx); cErr != nil {
			return Incident{}, false, fmt.Errorf("slo: open incident commit(existing): %w", cErr)
		}
		return existing, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, fmt.Errorf("slo: open incident select: %w", err)
	}

	in, err := scanIncident(tx.QueryRow(ctx, `
		INSERT INTO slo_incidents (slo_id, project_id, burn_rate, budget_remaining, in_maintenance)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+incidentColumns,
		sloID, projectID, burnRate, budgetRemaining, inMaintenance))
	if err != nil {
		// Гонка: параллельный INSERT уже создал открытый — перечитываем победителя.
		if isUniqueViolation(err) {
			_ = tx.Rollback(ctx)
			won, found, ferr := s.OpenIncidentFor(ctx, sloID)
			if ferr != nil {
				return Incident{}, false, ferr
			}
			if !found {
				return Incident{}, false, fmt.Errorf("slo: open incident: conflicted but no open incident found")
			}
			return won, false, nil
		}
		return Incident{}, false, fmt.Errorf("slo: open incident insert: %w", err)
	}
	if cErr := tx.Commit(ctx); cErr != nil {
		return Incident{}, false, fmt.Errorf("slo: open incident commit: %w", cErr)
	}
	return in, true, nil
}

func (s *Store) ResolveIncident(ctx context.Context, sloID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE slo_incidents SET status = 'resolved', resolved_at = now()
		WHERE slo_id = $1 AND status = 'open'
		RETURNING `+incidentColumns, sloID)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("slo: resolve incident: %w", err)
	}
	return in, true, nil
}

func (s *Store) MarkNotified(ctx context.Context, incidentID int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	_, err := s.pool.Exec(ctx,
		"UPDATE slo_incidents SET "+column+" = true WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("slo: mark notified: %w", err)
	}
	return nil
}

// project_id в WHERE — defense-in-depth: не даёт подтвердить чужой инцидент по id.
func (s *Store) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE slo_incidents SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND project_id = $2 AND status = 'open' AND acknowledged_at IS NULL
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("slo: acknowledge incident: %w", err)
	}
	return true, nil
}

// должен совпадать с incident_source 'slo' в incident_escalations.
func (s *Store) Name() string { return "slo" }

// члены открытых групп исключены — корень уведомляет за них; StartedAt =
// GREATEST(started, group.resolved) — иначе бывший член огрёб бы всю лесенку разом.
func (s *Store) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.project_id,
		       GREATEST(i.started_at, COALESCE(g.resolved_at, i.started_at)) AS started_at,
		       i.severity, i.escalation_level
		FROM slo_incidents i
		LEFT JOIN incident_groups g ON g.id = i.group_id
		WHERE i.status = 'open' AND i.acknowledged_at IS NULL
		  AND (i.group_id IS NULL OR g.id IS NULL OR g.resolved_at IS NOT NULL)
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("slo: open unacked incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("slo: open unacked incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// level=from — оптимистическая блокировка: false значит, другой тик уже продвинул эскалацию.
func (s *Store) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE slo_incidents SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("slo: bump escalation: %w", err)
	}
	return true, nil
}

func (s *Store) Incidents(ctx context.Context, projectID, sloID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM slo_incidents WHERE project_id = $1 AND slo_id = $2 ORDER BY started_at DESC LIMIT $3",
		projectID, sloID, limit)
	if err != nil {
		return nil, fmt.Errorf("slo: incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("slo: incidents scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// SQLSTATE 23505.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
