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
}

const incidentColumns = `id, monitor_id, started_at, resolved_at, cause, regions, in_maintenance, notified_open, notified_close, last_reminded_at`

func scanIncident(row pgx.Row) (Incident, error) {
	var inc Incident
	if err := row.Scan(&inc.ID, &inc.MonitorID, &inc.StartedAt, &inc.ResolvedAt, &inc.Cause,
		&inc.Regions, &inc.InMaintenance, &inc.NotifiedOpen, &inc.NotifiedClose, &inc.LastRemindedAt); err != nil {
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
			i.in_maintenance, i.notified_open, i.notified_close, i.last_reminded_at
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
