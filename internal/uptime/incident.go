package uptime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Incident — период недоступности монитора.
type Incident struct {
	ID             int64
	MonitorID      int64
	StartedAt      time.Time
	ResolvedAt     *time.Time
	Cause          string
	Regions        []string
	InMaintenance  bool
	NotifiedOpen   bool
	NotifiedClose  bool
	LastRemindedAt *time.Time
	// SuppressedByDep — инцидент подавлен, потому что у него есть упавший
	// задекларированный родитель (alert_dependencies, B5). Единственный
	// писатель — Service.MarkSuppressedByDep, вызываемый depsuppress.Suppressor
	// (source="uptime" резолвится в самом uptime, не в Suppressor — см. T7).
	SuppressedByDep bool
}

const incidentColumns = `id, monitor_id, started_at, resolved_at, cause, regions, in_maintenance, notified_open, notified_close, last_reminded_at, suppressed_by_dep`

func scanIncident(row pgx.Row) (Incident, error) {
	var inc Incident
	if err := row.Scan(&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause,
		&inc.Regions, &inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt,
		&inc.SuppressedByDep); err != nil {
		return Incident{}, err
	}
	return inc, nil
}

// OpenIncident opens a new incident for monitorID, unless one is already
// open. Race-safety relies on the partial unique index
// incidents_one_open_idx (monitor_id) WHERE resolved_at IS NULL: the INSERT
// targets that index directly, so of two concurrent callers exactly one
// INSERT succeeds and the other observes the conflict (DO NOTHING ->
// RETURNING yields no row) — no read-then-write race window. The loser then
// reads back the winner's incident and reports created=false.
func (s *Service) OpenIncident(ctx context.Context, monitorID int64, cause string, regions []string, inMaintenance bool) (Incident, bool, error) {
	if regions == nil {
		regions = []string{} // regions is NOT NULL; pgx encodes a nil slice as SQL NULL
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO incidents (monitor_id, cause, regions, in_maintenance)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (monitor_id) WHERE resolved_at IS NULL DO NOTHING
		RETURNING `+incidentColumns,
		monitorID, cause, regions, inMaintenance)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenIncidentFor(ctx, monitorID)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("uptime: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: open incident: %w", err)
	}
	return inc, true, nil
}

// ResolveIncident closes the currently open incident for monitorID, if any.
// ok=false when there was nothing open (idempotent: a second call after the
// first resolve reports ok=false rather than erroring).
func (s *Service) ResolveIncident(ctx context.Context, monitorID int64, at time.Time) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE incidents SET resolved_at = $2
		WHERE monitor_id = $1 AND resolved_at IS NULL
		RETURNING `+incidentColumns,
		monitorID, at)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: resolve incident: %w", err)
	}
	return inc, true, nil
}

// OpenIncidentFor returns the currently open incident for monitorID, if any.
func (s *Service) OpenIncidentFor(ctx context.Context, monitorID int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+incidentColumns+`
		FROM incidents WHERE monitor_id = $1 AND resolved_at IS NULL`, monitorID)
	inc, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("uptime: open incident for: %w", err)
	}
	return inc, true, nil
}

func queryIncidents(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) ([]Incident, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("uptime: incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		inc, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("uptime: incidents: %w", err)
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// Incidents returns the most recent incidents across all of projectID's
// monitors, freshest first.
func (s *Service) Incidents(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	return queryIncidents(ctx, s.pool, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE m.project_id = $1
		ORDER BY i.started_at DESC
		LIMIT $2`, projectID, limit)
}

// IncidentsForMonitor returns the most recent incidents for monitorID,
// freshest first.
func (s *Service) IncidentsForMonitor(ctx context.Context, monitorID int64, limit int) ([]Incident, error) {
	return queryIncidents(ctx, s.pool, `
		SELECT `+incidentColumns+`
		FROM incidents WHERE monitor_id = $1
		ORDER BY started_at DESC
		LIMIT $2`, monitorID, limit)
}

// IncidentsForMonitorsBatch — то же, что IncidentsForMonitor, но для набора
// monitorIDs одним запросом: до limit самых свежих инцидентов НА КАЖДЫЙ
// монитор (не limit суммарно на весь набор), тем же способом, каким
// IncidentsForMonitor режет по одному монитору. Публичная статус-страница
// иначе звала IncidentsForMonitor в цикле — третий и последний из трёх
// поштучных запросов на монитор, который ещё оставался в её сборке.
//
// row_number() OVER (PARTITION BY monitor_id ...) — оконная функция, а не
// LIMIT: обычный общий LIMIT после ORDER BY started_at DESC срезал бы топ-N
// по всему набору сразу, и монитор с активной историей инцидентов вытеснил
// бы из выдачи более тихие мониторы вместо того, чтобы у каждого был свой
// потолок в limit записей — именно так себя вёл бы цикл, который заменяем.
//
// Карта заполняется для ВСЕХ monitorIDs (тот же приём, что и в GetBatch/
// StatesBatch): монитор без единого инцидента присутствует с nil-слайсом, а
// не отсутствует вовсе — вызывающему не нужно отдельно проверять comma-ok.
func (s *Service) IncidentsForMonitorsBatch(ctx context.Context, monitorIDs []int64, limit int) (map[int64][]Incident, error) {
	out := make(map[int64][]Incident, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}
	for _, id := range monitorIDs {
		out[id] = nil
	}
	if limit <= 0 {
		return out, nil
	}
	rows, err := queryIncidents(ctx, s.pool, `
		SELECT `+incidentColumns+`
		FROM (
			SELECT `+incidentColumns+`,
			       row_number() OVER (PARTITION BY monitor_id ORDER BY started_at DESC) AS rn
			FROM incidents
			WHERE monitor_id = ANY($1)
		) ranked
		WHERE rn <= $2
		ORDER BY monitor_id, started_at DESC`, monitorIDs, limit)
	if err != nil {
		return nil, err
	}
	for _, inc := range rows {
		out[inc.MonitorID] = append(out[inc.MonitorID], inc)
	}
	return out, nil
}

// queryIncidentsPaged runs an incident query whose LAST selected column is
// count(*) OVER() AS total, returning the page rows together with the
// unpaginated total (0 for an empty page — no row carries the window count).
func queryIncidentsPaged(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) ([]Incident, int64, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("uptime: incidents: %w", err)
	}
	defer rows.Close()
	var out []Incident
	var total int64
	for rows.Next() {
		var inc Incident
		if err := rows.Scan(&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause,
			&inc.Regions, &inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt,
			&inc.SuppressedByDep, &total); err != nil {
			return nil, 0, fmt.Errorf("uptime: incidents: %w", err)
		}
		out = append(out, inc)
	}
	return out, total, rows.Err()
}

// IncidentsPaged returns one page (limit/offset) of projectID's incidents,
// freshest first, plus the total across all pages for the pager.
func (s *Service) IncidentsPaged(ctx context.Context, projectID int64, limit, offset int) ([]Incident, int64, error) {
	return queryIncidentsPaged(ctx, s.pool, `
		SELECT i.id, i.monitor_id, i.started_at, i.resolved_at, i.cause, i.regions,
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at, i.suppressed_by_dep,
			count(*) OVER() AS total
		FROM incidents i
		JOIN monitors m ON m.id = i.monitor_id
		WHERE m.project_id = $1
		ORDER BY i.started_at DESC
		LIMIT $2 OFFSET $3`, projectID, limit, offset)
}

// IncidentsForMonitorPaged returns one page (limit/offset) of monitorID's
// incidents, freshest first, plus the total across all pages.
func (s *Service) IncidentsForMonitorPaged(ctx context.Context, monitorID int64, limit, offset int) ([]Incident, int64, error) {
	return queryIncidentsPaged(ctx, s.pool, `
		SELECT `+incidentColumns+`, count(*) OVER() AS total
		FROM incidents WHERE monitor_id = $1
		ORDER BY started_at DESC
		LIMIT $2 OFFSET $3`, monitorID, limit, offset)
}

// MarkNotified records that an open/close notification was sent for an
// incident: open=true sets notified_open, otherwise notified_close.
func (s *Service) MarkNotified(ctx context.Context, incidentID int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE incidents SET "+column+" = true WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkSuppressedByDep records that incidentID was suppressed because a
// declared parent (alert_dependencies, B5) is itself down: source="uptime"
// resolution lives in this package (T7), unlike host incidents, whose sole
// writer for suppressed_by_dep is depsuppress.Suppressor.MarkSuppressed —
// see that method's doc comment for why the flag has exactly one writer per
// table.
func (s *Service) MarkSuppressedByDep(ctx context.Context, incidentID int64) error {
	tag, err := s.pool.Exec(ctx, "UPDATE incidents SET suppressed_by_dep = true WHERE id = $1", incidentID)
	if err != nil {
		return fmt.Errorf("uptime: mark suppressed by dep: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
