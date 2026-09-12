package host

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Host struct {
	ID           int64
	ProjectID    int64
	Name         string
	FirstSeen    time.Time
	LastSeen     time.Time
	AgentVersion string
	Environment  string
	Role         string
}

const MaxHostsPerProject = 1000

const MaxActiveHostsPerTick = 20_000

const hostColumns = `id, project_id, name, first_seen, last_seen, COALESCE(agent_version, ''), environment, role`

func validName(name string) bool {
	return name != "" && name != "." && name != ".."
}

func scanHost(row pgx.Row) (Host, error) {
	var h Host
	err := row.Scan(&h.ID, &h.ProjectID, &h.Name, &h.FirstSeen, &h.LastSeen, &h.AgentVersion, &h.Environment, &h.Role)
	return h, err
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Потолок и вставка — один CTE: count читается в снимке транзакции, окно гонки сужено, не закрыто.
// Место для новых имён — по порядку прихода в батче, потолок режет последних, не по алфавиту.
func (s *Store) Upsert(ctx context.Context, projectID int64, entries []TouchEntry) (int, error) {
	idx := make(map[string]int, len(entries))
	dedup := make([]TouchEntry, 0, len(entries))
	for _, e := range entries {
		if !validName(e.Name) {
			continue
		}
		if i, ok := idx[e.Name]; ok {
			if e.AgentVersion != "" {
				dedup[i].AgentVersion = e.AgentVersion
			}
			if e.Environment != "" {
				dedup[i].Environment = e.Environment
			}
			if e.Role != "" {
				dedup[i].Role = e.Role
			}
			continue
		}
		idx[e.Name] = len(dedup)
		dedup = append(dedup, e)
	}
	if len(dedup) == 0 {
		return 0, nil
	}
	names := make([]string, len(dedup))
	versions := make([]string, len(dedup))
	envs := make([]string, len(dedup))
	roles := make([]string, len(dedup))
	for i, e := range dedup {
		names[i] = e.Name
		versions[i] = e.AgentVersion
		envs[i] = e.Environment
		roles[i] = e.Role
	}
	tag, err := s.pool.Exec(ctx, `
		WITH input AS (
			SELECT DISTINCT ON (i.name) i.name, i.agent_version, i.environment, i.role, i.ord
			  FROM unnest($2::text[], $4::text[], $5::text[], $6::text[])
			       WITH ORDINALITY AS i(name, agent_version, environment, role, ord)
			 ORDER BY i.name, i.ord
		),
		room AS (
			SELECT GREATEST($3::bigint - count(*), 0) AS free FROM hosts WHERE project_id = $1
		),
		allowed AS (
			SELECT i.name, i.agent_version, i.environment, i.role FROM input i
			 WHERE EXISTS (SELECT 1 FROM hosts h WHERE h.project_id = $1 AND h.name = i.name)
			UNION ALL
			(SELECT i.name, i.agent_version, i.environment, i.role FROM input i
			  WHERE NOT EXISTS (SELECT 1 FROM hosts h WHERE h.project_id = $1 AND h.name = i.name)
			  ORDER BY i.ord
			  LIMIT (SELECT free FROM room))
		)
		INSERT INTO hosts (project_id, name, agent_version, environment, role)
		SELECT $1, name, NULLIF(agent_version, ''), environment, role FROM allowed
		ON CONFLICT (project_id, name) DO UPDATE SET
			last_seen = now(),
			agent_version = CASE WHEN EXCLUDED.agent_version <> '' THEN EXCLUDED.agent_version ELSE hosts.agent_version END,
			environment   = CASE WHEN EXCLUDED.environment   <> '' THEN EXCLUDED.environment   ELSE hosts.environment END,
			role          = CASE WHEN EXCLUDED.role          <> '' THEN EXCLUDED.role          ELSE hosts.role END`,
		projectID, names, MaxHostsPerProject, versions, envs, roles)
	if err != nil {
		return 0, fmt.Errorf("host: upsert: %w", err)
	}
	rejected := len(dedup) - int(tag.RowsAffected())
	if rejected < 0 {
		rejected = 0
	}
	return rejected, nil
}

func (s *Store) List(ctx context.Context, projectID int64, limit int) ([]Host, error) {
	if limit <= 0 {
		limit = MaxHostsPerProject
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE project_id = $1 ORDER BY name LIMIT $2", projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

const HostLabelNone = "__none__"

type HostFilter struct {
	Environment string
	Role        string
	NewOnly     bool
}

func (s *Store) ListFiltered(ctx context.Context, projectID int64, f HostFilter, limit int) ([]Host, error) {
	if limit <= 0 {
		limit = MaxHostsPerProject
	}
	q := "SELECT " + hostColumns + " FROM hosts WHERE project_id = $1"
	args := []any{projectID}
	add := func(col, val string) {
		if val == "" {
			return
		}
		if val == HostLabelNone {
			q += " AND " + col + " = ''"
			return
		}
		args = append(args, val)
		q += fmt.Sprintf(" AND %s = $%d", col, len(args))
	}
	add("environment", f.Environment)
	add("role", f.Role)
	if f.NewOnly {
		q += " AND first_seen > now() - interval '24 hours'"
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY name LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("host: list filtered: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list filtered scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) FacetValues(ctx context.Context, projectID int64) (envs, roles []string, err error) {
	envs, err = s.distinctLabelValues(ctx, projectID, "environment")
	if err != nil {
		return nil, nil, err
	}
	roles, err = s.distinctLabelValues(ctx, projectID, "role")
	if err != nil {
		return nil, nil, err
	}
	return envs, roles, nil
}

func (s *Store) distinctLabelValues(ctx context.Context, projectID int64, col string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT DISTINCT "+col+" FROM hosts WHERE project_id = $1 AND "+col+" <> '' ORDER BY 1", projectID)
	if err != nil {
		return nil, fmt.Errorf("host: facet values %s: %w", col, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("host: facet values %s scan: %w", col, err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) Get(ctx context.Context, projectID int64, name string) (Host, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE project_id = $1 AND name = $2", projectID, name)
	h, err := scanHost(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, false, nil
	}
	if err != nil {
		return Host{}, false, fmt.Errorf("host: get: %w", err)
	}
	return h, true, nil
}

func (s *Store) ListByIDs(ctx context.Context, ids []int64) ([]Host, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE id = ANY($1)", ids)
	if err != nil {
		return nil, fmt.Errorf("host: list by ids: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list by ids scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) Delete(ctx context.Context, projectID int64, name string) (bool, error) {
	row := s.pool.QueryRow(ctx,
		"DELETE FROM hosts WHERE project_id = $1 AND name = $2 RETURNING id", projectID, name)
	var id int64
	err := row.Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("host: delete: %w", err)
	}
	return true, nil
}

func (s *Store) ListActiveWithProject(ctx context.Context, freshWithin time.Duration, limit int) ([]Host, error) {
	if limit <= 0 {
		limit = MaxActiveHostsPerTick
	}
	since := time.Now().Add(-freshWithin)
	rows, err := s.pool.Query(ctx,
		"SELECT "+hostColumns+" FROM hosts WHERE last_seen > $1 ORDER BY project_id, name LIMIT $2", since, limit)
	if err != nil {
		return nil, fmt.Errorf("host: list active: %w", err)
	}
	defer rows.Close()
	var out []Host
	for rows.Next() {
		h, err := scanHost(rows)
		if err != nil {
			return nil, fmt.Errorf("host: list active scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
