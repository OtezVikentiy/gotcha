package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/escalation"
)

var Kinds = []string{"disk", "memory", "load", "silent"}

var ErrIncidentNotFound = errors.New("host: incident not found")

type Incident struct {
	ID              int64
	ProjectID       int64
	HostID          int64
	Kind            string
	Status          string
	CurrentValue    float64
	PeakValue       float64
	Detail          string
	StartedAt       time.Time
	ResolvedAt      *time.Time
	InMaintenance   bool
	NotifiedOpen    bool
	NotifiedClose   bool
	AcknowledgedAt  *time.Time
	AcknowledgedBy  *int64
	Severity        string
	SuppressedByDep bool
}

const incidentColumns = `id, project_id, host_id, kind, status, current_value, peak_value,
	detail, started_at, resolved_at, in_maintenance, notified_open, notified_close,
	acknowledged_at, acknowledged_by, severity, suppressed_by_dep`

func scanIncident(row pgx.Row) (Incident, error) {
	var in Incident
	err := row.Scan(&in.ID, &in.ProjectID, &in.HostID, &in.Kind, &in.Status,
		&in.CurrentValue, &in.PeakValue, &in.Detail,
		&in.StartedAt, &in.ResolvedAt, &in.InMaintenance, &in.NotifiedOpen, &in.NotifiedClose,
		&in.AcknowledgedAt, &in.AcknowledgedBy, &in.Severity, &in.SuppressedByDep)
	return in, err
}

type IncidentService struct {
	pool *pgxpool.Pool
}

func NewIncidentService(pool *pgxpool.Pool) *IncidentService {
	return &IncidentService{pool: pool}
}

// Гонко-безопасно частичным уникальным индексом (host_id, kind) WHERE status='open':
// параллельный INSERT ловит конфликт, RETURNING пуст, победителя дочитывает OpenFor.
func (s *IncidentService) Open(ctx context.Context, projectID, hostID int64, kind string, current float64, detail string, inMaintenance bool) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO host_incidents (project_id, host_id, kind, status, peak_value, current_value, detail, in_maintenance)
		VALUES ($1, $2, $3, 'open', $4, $4, $5, $6)
		ON CONFLICT (host_id, kind) WHERE status = 'open' DO NOTHING
		RETURNING `+incidentColumns,
		projectID, hostID, kind, current, detail, inMaintenance)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		existing, found, err := s.OpenFor(ctx, hostID, kind)
		if err != nil {
			return Incident{}, false, err
		}
		if !found {
			return Incident{}, false, fmt.Errorf("host: open incident: conflicted but no open incident found")
		}
		return existing, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("host: open incident: %w", err)
	}
	return in, true, nil
}

func (s *IncidentService) OpenFor(ctx context.Context, hostID int64, kind string) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE host_id = $1 AND kind = $2 AND status = 'open'",
		hostID, kind)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("host: open incident for: %w", err)
	}
	return in, true, nil
}

func (s *IncidentService) GetByID(ctx context.Context, id int64) (Incident, bool, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+incidentColumns+" FROM host_incidents WHERE id = $1", id)
	in, err := scanIncident(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Incident{}, false, nil
	}
	if err != nil {
		return Incident{}, false, fmt.Errorf("host: get incident by id: %w", err)
	}
	return in, true, nil
}

func (s *IncidentService) Bump(ctx context.Context, id int64, current, peak float64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET current_value = $2, peak_value = $3
		WHERE id = $1 AND status = 'open'`, id, current, peak)
	if err != nil {
		return fmt.Errorf("host: bump incident: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

func (s *IncidentService) Resolve(ctx context.Context, id int64, current float64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now(), current_value = $2
		WHERE id = $1 AND status = 'open'
		RETURNING id`, id, current)
	var closedID int64
	err := row.Scan(&closedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("host: resolve incident: %w", err)
	}
	return true, nil
}

func (s *IncidentService) ResolveOpenByProjectKind(ctx context.Context, projectID int64, kind string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now()
		WHERE project_id = $1 AND kind = $2 AND status = 'open'`, projectID, kind)
	if err != nil {
		return 0, fmt.Errorf("host: resolve open incidents by project kind: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *IncidentService) ResolveOpenByHostKind(ctx context.Context, hostID int64, kind string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now()
		WHERE host_id = $1 AND kind = $2 AND status = 'open'`, hostID, kind)
	if err != nil {
		return 0, fmt.Errorf("host: resolve open incidents by host kind: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Один UPDATE на весь список хостов — вызывающий группирует по kind сам,
// не гонит round-trip на каждую пару «хост × вид».
func (s *IncidentService) ResolveOpenByHostsKind(ctx context.Context, hostIDs []int64, kind string) (int64, error) {
	if len(hostIDs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET status = 'resolved', resolved_at = now()
		WHERE host_id = ANY($1) AND kind = $2 AND status = 'open'`, hostIDs, kind)
	if err != nil {
		return 0, fmt.Errorf("host: resolve open incidents by hosts kind: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (s *IncidentService) ListOpenKindsForHosts(ctx context.Context, hostIDs []int64) (map[int64]map[string]bool, error) {
	out := make(map[int64]map[string]bool, len(hostIDs))
	if len(hostIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT host_id, kind FROM host_incidents WHERE host_id = ANY($1) AND status = 'open'`,
		hostIDs)
	if err != nil {
		return nil, fmt.Errorf("host: list open incident kinds for hosts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hostID int64
		var kind string
		if err := rows.Scan(&hostID, &kind); err != nil {
			return nil, fmt.Errorf("host: scan open incident kind row: %w", err)
		}
		if out[hostID] == nil {
			out[hostID] = make(map[string]bool)
		}
		out[hostID][kind] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("host: list open incident kinds for hosts: %w", err)
	}
	return out, nil
}

func (s *IncidentService) MarkNotified(ctx context.Context, id int64, open bool) error {
	column := "notified_close"
	if open {
		column = "notified_open"
	}
	tag, err := s.pool.Exec(ctx, "UPDATE host_incidents SET "+column+" = true WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("host: mark incident notified: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrIncidentNotFound
	}
	return nil
}

func (s *IncidentService) Acknowledge(ctx context.Context, incidentID, projectID, userID int64) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE host_incidents SET acknowledged_at = now(), acknowledged_by = $3
		WHERE id = $1 AND project_id = $2 AND status = 'open' AND acknowledged_at IS NULL
		RETURNING id`, incidentID, projectID, userID)
	var ackedID int64
	err := row.Scan(&ackedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("host: acknowledge incident: %w", err)
	}
	return true, nil
}

func (s *IncidentService) Name() string { return "host" }

// GREATEST по started_at/resolved_at/dep_released_at — анти-залп: часы лесенки эскалации
// перезапускаются от момента освобождения, а не от исходного открытия инцидента.
func (s *IncidentService) OpenUnacked(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.project_id,
		       GREATEST(i.started_at, COALESCE(g.resolved_at, i.started_at), COALESCE(i.dep_released_at, i.started_at)) AS started_at,
		       i.severity, i.escalation_level
		FROM host_incidents i
		LEFT JOIN incident_groups g ON g.id = i.group_id
		WHERE i.status = 'open' AND i.acknowledged_at IS NULL AND i.suppressed_by_dep = false
		  AND (i.group_id IS NULL OR g.id IS NULL OR g.resolved_at IS NOT NULL)
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("host: open unacked incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("host: open unacked incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *IncidentService) BumpEscalation(ctx context.Context, id int64, from int) (bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE host_incidents SET escalation_level = $2 + 1, last_escalated_at = now()
		WHERE id = $1 AND escalation_level = $2
		RETURNING id`, id, from)
	var bumpedID int64
	err := row.Scan(&bumpedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("host: bump escalation: %w", err)
	}
	return true, nil
}

func (s *IncidentService) OpenSuppressed(ctx context.Context) ([]escalation.PendingIncident, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.project_id, i.started_at, i.severity, i.escalation_level
		FROM host_incidents i
		WHERE i.status = 'open' AND i.acknowledged_at IS NULL AND i.suppressed_by_dep = true
		ORDER BY i.id`)
	if err != nil {
		return nil, fmt.Errorf("host: open suppressed incidents: %w", err)
	}
	defer rows.Close()
	var out []escalation.PendingIncident
	for rows.Next() {
		var p escalation.PendingIncident
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.StartedAt, &p.Severity, &p.EscalationLevel); err != nil {
			return nil, fmt.Errorf("host: open suppressed incidents scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *IncidentService) ClearSuppressed(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE host_incidents SET suppressed_by_dep = false, dep_released_at = now()
		WHERE id = $1 AND suppressed_by_dep`, id)
	if err != nil {
		return fmt.Errorf("host: clear suppressed: %w", err)
	}
	return nil
}

func (s *IncidentService) ListByProject(ctx context.Context, projectID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE project_id = $1 ORDER BY started_at DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list incidents by project: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list incidents by project scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *IncidentService) ListOpenByProject(ctx context.Context, projectID int64) ([]Incident, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE project_id = $1 AND status = 'open' ORDER BY started_at DESC",
		projectID)
	if err != nil {
		return nil, fmt.Errorf("host: list open incidents by project: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list open incidents by project scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *IncidentService) ListRecentByHost(ctx context.Context, hostID int64, limit int) ([]Incident, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE host_id = $1 ORDER BY started_at DESC LIMIT $2",
		hostID, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list recent incidents by host: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list recent incidents by host scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *IncidentService) ListOpenByHost(ctx context.Context, hostID int64) ([]Incident, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+incidentColumns+" FROM host_incidents WHERE host_id = $1 AND status = 'open' ORDER BY started_at DESC",
		hostID)
	if err != nil {
		return nil, fmt.Errorf("host: list open incidents by host: %w", err)
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list open incidents by host scan: %w", err)
		}
		out = append(out, in)
	}
	return out, rows.Err()
}
