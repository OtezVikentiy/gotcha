package org

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	PlatformGo         = "go"
	PlatformPHP        = "php"
	PlatformJavaScript = "javascript"
	PlatformPython     = "python"
	PlatformOther      = "other"
)

var Platforms = []string{PlatformGo, PlatformPHP, PlatformJavaScript, PlatformPython, PlatformOther}

var allowedPlatforms = func() map[string]bool {
	m := make(map[string]bool, len(Platforms))
	for _, p := range Platforms {
		m[p] = true
	}
	return m
}()

func NormalizePlatform(platform string) string {
	if allowedPlatforms[platform] {
		return platform
	}
	return PlatformOther
}

type Project struct {
	ID       int64
	OrgID    int64
	Slug     string
	Name     string
	Platform string

	TransactionSampleRate float64
	ApdexThresholdMS      int32
	PerfDetectorConfig    string

	PerfRegressionConfig string
}

const projectColumns = "id, org_id, slug, name, platform, " +
	"transaction_sample_rate, apdex_threshold_ms, perf_detector_config, perf_regression_config"

func scanProject(row pgx.Row) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.OrgID, &p.Slug, &p.Name, &p.Platform,
		&p.TransactionSampleRate, &p.ApdexThresholdMS, &p.PerfDetectorConfig, &p.PerfRegressionConfig)
	return p, err
}

func (s *Service) CreateProject(ctx context.Context, orgID int64, slug, name, platform string) (Project, error) {
	if !validSlug(slug) {
		return Project{}, ErrInvalidSlug
	}
	platform = NormalizePlatform(platform)
	p, err := scanProject(s.pool.QueryRow(ctx,
		"INSERT INTO projects (org_id, slug, name, platform) VALUES ($1, $2, $3, $4) RETURNING "+projectColumns,
		orgID, slug, name, platform))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return Project{}, ErrSlugTaken
	}
	if err != nil {
		return Project{}, fmt.Errorf("org: create project: %w", err)
	}
	return p, nil
}

func (s *Service) AttachTeam(ctx context.Context, projectID, teamID int64) error {
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO project_teams (project_id, team_id) VALUES ($1, $2) ON CONFLICT DO NOTHING",
		projectID, teamID); err != nil {
		return fmt.Errorf("org: attach team: %w", err)
	}
	return nil
}

func (s *Service) DetachTeam(ctx context.Context, projectID, teamID int64) error {
	if _, err := s.pool.Exec(ctx,
		"DELETE FROM project_teams WHERE project_id = $1 AND team_id = $2",
		projectID, teamID); err != nil {
		return fmt.Errorf("org: detach team: %w", err)
	}
	return nil
}

func (s *Service) RenameProject(ctx context.Context, projectID int64, name string) error {
	if name == "" {
		return ErrInvalidName
	}
	tag, err := s.pool.Exec(ctx,
		"UPDATE projects SET name = $2 WHERE id = $1", projectID, name)
	if err != nil {
		return fmt.Errorf("org: rename project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) UpdatePerfSettings(ctx context.Context, projectID int64, sampleRate float64, apdexMS int32, detectorConfigJSON string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE projects SET transaction_sample_rate = $1, apdex_threshold_ms = $2, perf_detector_config = $3 WHERE id = $4",
		sampleRate, apdexMS, detectorConfigJSON, projectID)
	if err != nil {
		return fmt.Errorf("org: update perf settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) UpdateRegressionConfig(ctx context.Context, projectID int64, configJSON string) error {
	tag, err := s.pool.Exec(ctx,
		"UPDATE projects SET perf_regression_config = $1 WHERE id = $2",
		configJSON, projectID)
	if err != nil {
		return fmt.Errorf("org: update regression config: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) DeleteProject(ctx context.Context, projectID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("org: delete project: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`INSERT INTO project_purge_queue (project_id) VALUES ($1)
		 ON CONFLICT (project_id) DO NOTHING`, projectID); err != nil {
		return fmt.Errorf("org: delete project: enqueue purge: %w", err)
	}
	tag, err := tx.Exec(ctx, "DELETE FROM projects WHERE id = $1", projectID)
	if err != nil {
		return fmt.Errorf("org: delete project: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("org: delete project: %w", err)
	}
	return nil
}

// Внешние скобки обязательны: без них "...AND "+accessCondition разбирается как
// (условие1 AND первая_ветвь) OR вторая_ветвь — вторая теряет сужение по org_id.
const accessCondition = `(
	EXISTS (
		SELECT 1 FROM org_members m
		WHERE m.org_id = p.org_id AND m.user_id = $1 AND m.role IN ('owner','admin')
	) OR EXISTS (
		SELECT 1 FROM project_teams pt
		JOIN team_members tm ON tm.team_id = pt.team_id
		JOIN org_members m2 ON m2.org_id = p.org_id AND m2.user_id = tm.user_id
		WHERE pt.project_id = p.id AND tm.user_id = $1
	)
)`

func (s *Service) ProjectsForUser(ctx context.Context, userID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+projectColumns+" FROM projects p WHERE "+
			accessCondition+" ORDER BY p.id", userID)
	if err != nil {
		return nil, fmt.Errorf("org: projects for user: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("org: projects for user: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) ProjectsForUserInOrg(ctx context.Context, userID, orgID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+projectColumns+" FROM projects p WHERE p.org_id = $2 AND "+
			accessCondition+" ORDER BY p.id", userID, orgID)
	if err != nil {
		return nil, fmt.Errorf("org: projects for user in org: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("org: projects for user in org: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) GetProject(ctx context.Context, projectID int64) (Project, error) {
	p, err := scanProject(s.pool.QueryRow(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE id = $1", projectID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("org: get project: %w", err)
	}
	return p, nil
}

func (s *Service) ProjectsOf(ctx context.Context, orgID int64) ([]Project, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+projectColumns+" FROM projects WHERE org_id = $1 ORDER BY name", orgID)
	if err != nil {
		return nil, fmt.Errorf("org: projects of: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("org: projects of: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) ProjectOrg(ctx context.Context, projectID int64) (int64, error) {
	var orgID int64
	err := s.pool.QueryRow(ctx, "SELECT org_id FROM projects WHERE id = $1", projectID).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("org: project org: %w", err)
	}
	return orgID, nil
}

func (s *Service) CanAccessProject(ctx context.Context, userID, projectID int64) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx,
		"SELECT "+accessCondition+" FROM projects p WHERE p.id = $2",
		userID, projectID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("org: can access project: %w", err)
	}
	return ok, nil
}
