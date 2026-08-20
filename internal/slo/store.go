package slo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxName — максимум рун в имени SLO (капается по рунам, не по байтам).
const maxName = 200

// maxSLOsPerProject — верхний предел числа SLO на проект. Кап-на-проект — главная
// защита от раздувания: без него оператор плодит latency-SLO (raw-скан ~10с
// каждый), а единый последовательный оценщик (ListEnabled) растягивает тик за
// интервал → error-budget алерты задерживаются у ВСЕХ тенантов (cross-tenant
// blast radius).
const maxSLOsPerProject = 100

// ErrTooManySLOs — достигнут предел числа SLO на проект. Экспортируется, чтобы
// web-слой отличил её от прочих ошибок и отдал 422, а не 500.
var ErrTooManySLOs = errors.New("slo: too many slos for project")

// capStr — обрезка строки до n рун. Имя НЕ `cap`: тот шадовит builtin.
func capStr(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// Store — Postgres-стор определений SLO (таблица slos) и инцидентов сжигания
// бюджета (slo_incidents). Все запросы скоупятся по project_id (тенант-изоляция),
// кроме ListEnabled — тот читает все включённые SLO для оценщика.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore создаёт стор поверх пула pgx.
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

// Create валидирует минимально (капы длины) и создаёт SLO. Возвращает
// вставленную строку со всеми DEFAULT-полями.
func (s *Store) Create(ctx context.Context, in SLO) (SLO, error) {
	in.Name = capStr(in.Name, maxName)
	in.Transaction = capStr(in.Transaction, maxName)
	in.Environment = capStr(in.Environment, maxName)
	// Кап-на-проект: считаем существующие до вставки. При гонке на границе
	// возможен небольшой перелёт, но это не защита безопасности, а ограничение
	// blast-radius оценщика — точность до единицы не требуется.
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

// Get возвращает SLO проекта по id (found=false, если нет или чужой проект).
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

// List возвращает SLO проекта, свежайшие первыми.
func (s *Store) List(ctx context.Context, projectID int64) ([]SLO, error) {
	// LIMIT с запасом над капом-на-проект (maxSLOsPerProject=100) — defence in
	// depth: даже если кап обойдён (гонка/ручная вставка), список не станет O(N).
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

// ListEnabled возвращает все включённые SLO по всем проектам — для оценщика.
func (s *Store) ListEnabled(ctx context.Context) ([]SLO, error) {
	// LIMIT — вторичная защита от неограниченного прохода оценщика на инсталляции
	// с тысячами проектов (кап-на-проект ограничивает каждый проект, но не их
	// число). Потолок с большим запасом: 5000 включённых SLO — уже за гранью
	// разумного для одного оценочного тика.
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

// Delete удаляет SLO проекта (scoped по projectID — чужое не удалить). Инциденты
// уходят каскадом (ON DELETE CASCADE).
func (s *Store) Delete(ctx context.Context, projectID, id int64) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM slos WHERE project_id = $1 AND id = $2", projectID, id)
	if err != nil {
		return fmt.Errorf("slo: delete: %w", err)
	}
	return nil
}

const incidentColumns = `id, slo_id, project_id, status, burn_rate, budget_remaining,
	started_at, resolved_at, in_maintenance, notified_open, notified_close,
	acknowledged_at, acknowledged_by`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.SLOID, &in.ProjectID, &in.Status, &in.BurnRate, &in.BudgetRemaining,
		&in.StartedAt, &in.ResolvedAt, &in.InMaintenance, &in.NotifiedOpen, &in.NotifiedClose,
		&in.AcknowledgedAt, &in.AcknowledgedBy)
	return in, err
}

// OpenIncidentFor возвращает открытый инцидент SLO, если он есть.
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

// OpenIncident открывает инцидент сжигания бюджета, если открытого ещё нет
// (модель one-open через частичный уникальный индекс slo_incidents_one_open_idx).
// created=true — вставлен новый; created=false — уже был открытый (вернётся он).
// Выполняется в транзакции: SELECT открытого → если есть, вернуть; иначе INSERT.
// При гонке параллельный INSERT ловит уникальное нарушение → перечитываем победителя.
func (s *Store) OpenIncident(ctx context.Context, sloID, projectID int64, burnRate float64, budgetRemaining *float64, inMaintenance bool) (Incident, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Incident{}, false, fmt.Errorf("slo: open incident begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Уже есть открытый — вернуть его.
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

	// Открытого нет — вставляем.
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

// ResolveIncident закрывает открытый инцидент SLO. resolved=false, если открытого
// не было (идемпотентно).
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

// MarkNotified фиксирует отправку уведомления (open → notified_open, иначе
// notified_close).
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

// Acknowledge подтверждает открытый инцидент (B4: эскалации) — фиксирует
// acknowledged_at/acknowledged_by, чем гасит дальнейшую эскалацию. ok=false,
// если инцидент уже подтверждён или закрыт (идемпотентно).
func (s *Store) Acknowledge(ctx context.Context, incidentID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE slo_incidents SET acknowledged_at = now(), acknowledged_by = $2
		WHERE id = $1 AND status = 'open' AND acknowledged_at IS NULL
		RETURNING id`, incidentID, userID)
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

// Incidents возвращает инциденты SLO в проекте, свежайшие первыми.
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

// isUniqueViolation — true, если ошибка pgx — нарушение уникального индекса (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
