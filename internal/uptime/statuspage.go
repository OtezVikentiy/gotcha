package uptime

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

var (
	ErrInvalidStatusPage = errors.New("uptime: invalid status page")
	ErrSlugTaken         = errors.New("uptime: slug already taken")
)

// StatusPage — публичная страница статуса проекта.
type StatusPage struct {
	ID          int64
	ProjectID   int64
	Slug        string
	Title       string
	Description string
	Enabled     bool
}

// StatusPageMonitor — монитор, показанный на статус-странице.
type StatusPageMonitor struct {
	MonitorID   int64
	DisplayName string
	Position    int
}

func validateStatusPage(sp StatusPage) error {
	if !org.ValidSlug(sp.Slug) {
		return fmt.Errorf("%w: invalid slug", ErrInvalidStatusPage)
	}
	if sp.Title == "" {
		return fmt.Errorf("%w: title must not be empty", ErrInvalidStatusPage)
	}
	return nil
}

func replaceStatusPageMonitors(ctx context.Context, tx pgx.Tx, statusPageID int64, monitors []StatusPageMonitor) error {
	if _, err := tx.Exec(ctx, "DELETE FROM status_page_monitors WHERE status_page_id = $1", statusPageID); err != nil {
		return fmt.Errorf("uptime: replace status page monitors: %w", err)
	}
	for _, m := range monitors {
		if _, err := tx.Exec(ctx, `
			INSERT INTO status_page_monitors (status_page_id, monitor_id, display_name, position)
			VALUES ($1,$2,$3,$4)`, statusPageID, m.MonitorID, m.DisplayName, m.Position); err != nil {
			return fmt.Errorf("uptime: replace status page monitors: %w", err)
		}
	}
	return nil
}

// CreateStatusPage creates a status page together with its monitor list in
// one transaction. slug follows org.ValidSlug; a taken slug (globally
// unique) yields ErrSlugTaken.
func (s *Service) CreateStatusPage(ctx context.Context, sp StatusPage, monitors []StatusPageMonitor) (StatusPage, error) {
	if err := validateStatusPage(sp); err != nil {
		return StatusPage{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusPage{}, fmt.Errorf("uptime: create status page: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO status_pages (project_id, slug, title, description, enabled)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		sp.ProjectID, sp.Slug, sp.Title, sp.Description, sp.Enabled,
	).Scan(&sp.ID)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return StatusPage{}, ErrSlugTaken
	}
	if err != nil {
		return StatusPage{}, fmt.Errorf("uptime: create status page: %w", err)
	}

	if err := replaceStatusPageMonitors(ctx, tx, sp.ID, monitors); err != nil {
		return StatusPage{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return StatusPage{}, fmt.Errorf("uptime: create status page: %w", err)
	}
	return sp, nil
}

// UpdateStatusPage replaces a status page's fields and monitor list.
func (s *Service) UpdateStatusPage(ctx context.Context, sp StatusPage, monitors []StatusPageMonitor) error {
	if err := validateStatusPage(sp); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("uptime: update status page: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE status_pages SET slug=$2, title=$3, description=$4, enabled=$5
		WHERE id=$1`,
		sp.ID, sp.Slug, sp.Title, sp.Description, sp.Enabled)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrSlugTaken
	}
	if err != nil {
		return fmt.Errorf("uptime: update status page: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if err := replaceStatusPageMonitors(ctx, tx, sp.ID, monitors); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("uptime: update status page: %w", err)
	}
	return nil
}

// DeleteStatusPage deletes a status page by id.
func (s *Service) DeleteStatusPage(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, "DELETE FROM status_pages WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("uptime: delete status page: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// StatusPagesOf returns projectID's status pages (enabled and disabled),
// ordered by slug.
func (s *Service) StatusPagesOf(ctx context.Context, projectID int64) ([]StatusPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, slug, title, description, enabled
		FROM status_pages WHERE project_id = $1 ORDER BY slug`, projectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: status pages of: %w", err)
	}
	defer rows.Close()
	var out []StatusPage
	for rows.Next() {
		var sp StatusPage
		if err := rows.Scan(&sp.ID, &sp.ProjectID, &sp.Slug, &sp.Title, &sp.Description, &sp.Enabled); err != nil {
			return nil, fmt.Errorf("uptime: status pages of: %w", err)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// StatusPageBySlug returns an enabled status page and its monitors by slug
// (the public, unauthenticated lookup). Disabled pages and unknown slugs
// both yield ErrNotFound — a disabled page must be indistinguishable from
// one that doesn't exist.
func (s *Service) StatusPageBySlug(ctx context.Context, slug string) (StatusPage, []StatusPageMonitor, error) {
	var sp StatusPage
	err := s.pool.QueryRow(ctx, `
		SELECT id, project_id, slug, title, description, enabled
		FROM status_pages WHERE slug = $1 AND enabled = true`, slug,
	).Scan(&sp.ID, &sp.ProjectID, &sp.Slug, &sp.Title, &sp.Description, &sp.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusPage{}, nil, ErrNotFound
	}
	if err != nil {
		return StatusPage{}, nil, fmt.Errorf("uptime: status page by slug: %w", err)
	}

	monitors, err := s.StatusPageMonitors(ctx, sp.ID)
	if err != nil {
		return StatusPage{}, nil, err
	}
	return sp, monitors, nil
}

// StatusPageByID returns a status page by id regardless of enabled (the
// settings lookup: POST /statuspages/{id} resolves the owning project from
// the page itself, so a page of a foreign project can 404 without the caller
// having to trust a project id from the form).
func (s *Service) StatusPageByID(ctx context.Context, id int64) (StatusPage, error) {
	var sp StatusPage
	err := s.pool.QueryRow(ctx, `
		SELECT id, project_id, slug, title, description, enabled
		FROM status_pages WHERE id = $1`, id,
	).Scan(&sp.ID, &sp.ProjectID, &sp.Slug, &sp.Title, &sp.Description, &sp.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusPage{}, ErrNotFound
	}
	if err != nil {
		return StatusPage{}, fmt.Errorf("uptime: status page by id: %w", err)
	}
	return sp, nil
}

// StatusPageMonitors returns a status page's monitors ordered by position.
func (s *Service) StatusPageMonitors(ctx context.Context, statusPageID int64) ([]StatusPageMonitor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT monitor_id, display_name, position
		FROM status_page_monitors WHERE status_page_id = $1 ORDER BY position`, statusPageID)
	if err != nil {
		return nil, fmt.Errorf("uptime: status page monitors: %w", err)
	}
	defer rows.Close()
	var monitors []StatusPageMonitor
	for rows.Next() {
		var m StatusPageMonitor
		if err := rows.Scan(&m.MonitorID, &m.DisplayName, &m.Position); err != nil {
			return nil, fmt.Errorf("uptime: status page monitors: %w", err)
		}
		monitors = append(monitors, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: status page monitors: %w", err)
	}
	return monitors, nil
}
