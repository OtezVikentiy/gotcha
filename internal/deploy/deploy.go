package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeployedAt — момент выкладки по версии CI, CreatedAt — момент приёма записи.
type Deployment struct {
	ID          int64
	ProjectID   int64
	Version     string
	Environment string
	URL         string
	Changelog   string
	DeployedAt  time.Time
	CreatedAt   time.Time
}

// Защита горячего пути приёма от разросшихся полей (CI может прислать
// многокилобайтный changelog).
const (
	maxVersion     = 512
	maxEnvironment = 128
	maxURL         = 2048
	maxChangelog   = 16384
)

// Не `cap`: имя шадовило бы builtin.
func capStr(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

const deployColumns = `id, project_id, version, environment, url, changelog, deployed_at, created_at`

func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	err := row.Scan(&d.ID, &d.ProjectID, &d.Version, &d.Environment, &d.URL, &d.Changelog, &d.DeployedAt, &d.CreatedAt)
	return d, err
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Record(ctx context.Context, projectID int64, d Deployment) (Deployment, error) {
	if d.DeployedAt.IsZero() {
		d.DeployedAt = time.Now().UTC()
	}
	d.ProjectID = projectID
	d.Version = capStr(d.Version, maxVersion)
	d.Environment = capStr(d.Environment, maxEnvironment)
	d.URL = capStr(d.URL, maxURL)
	d.Changelog = capStr(d.Changelog, maxChangelog)
	row := s.pool.QueryRow(ctx,
		`INSERT INTO deployments (project_id, version, environment, deployed_at, url, changelog)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		projectID, d.Version, d.Environment, d.DeployedAt, d.URL, d.Changelog)
	if err := row.Scan(&d.ID, &d.CreatedAt); err != nil {
		return Deployment{}, fmt.Errorf("deploy: record: %w", err)
	}
	return d, nil
}

func (s *Store) List(ctx context.Context, projectID int64, from, to time.Time, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+deployColumns+" FROM deployments WHERE project_id = $1 AND deployed_at >= $2 AND deployed_at < $3 ORDER BY deployed_at DESC LIMIT $4",
		projectID, from, to, limit)
	if err != nil {
		return nil, fmt.Errorf("deploy: list: %w", err)
	}
	defer rows.Close()
	return scanDeployments(rows)
}

func (s *Store) Recent(ctx context.Context, projectID int64, limit int) ([]Deployment, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+deployColumns+" FROM deployments WHERE project_id = $1 ORDER BY deployed_at DESC LIMIT $2",
		projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("deploy: recent: %w", err)
	}
	defer rows.Close()
	return scanDeployments(rows)
}

func (s *Store) Nearest(ctx context.Context, projectID int64, before time.Time) (Deployment, bool, error) {
	row := s.pool.QueryRow(ctx,
		"SELECT "+deployColumns+" FROM deployments WHERE project_id = $1 AND deployed_at <= $2 ORDER BY deployed_at DESC LIMIT 1",
		projectID, before)
	d, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, false, nil
	}
	if err != nil {
		return Deployment{}, false, fmt.Errorf("deploy: nearest: %w", err)
	}
	return d, true, nil
}

func scanDeployments(rows pgx.Rows) ([]Deployment, error) {
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("deploy: scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
