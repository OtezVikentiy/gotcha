package uptime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrInvalidStatusPage = errors.New("uptime: invalid status page")

type StatusPage struct {
	ID          int64
	ProjectID   int64
	PublicID    string
	Title       string
	Description string
	Enabled     bool
}

type StatusPageMonitor struct {
	MonitorID   int64
	DisplayName string
	Position    int
}

// префикс "p_" отличает от legacy-slug при резолве; формат совпадает с
// DEFAULT колонки public_id (миграция 0062) для INSERT старым бинарём.
func newStatusPagePublicID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("uptime: status page public id: %w", err)
	}
	return "p_" + hex.EncodeToString(raw), nil
}

func validateStatusPage(sp StatusPage) error {
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

// используется, чтобы отличить настоящую коллизию public_id от любой
// другой 23505-ошибки внутри той же попытки — см. CreateStatusPage.
const statusPagePublicIDConstraint = "status_pages_public_id_key"

func (s *Service) CreateStatusPage(ctx context.Context, sp StatusPage, monitors []StatusPageMonitor) (StatusPage, error) {
	if err := validateStatusPage(sp); err != nil {
		return StatusPage{}, err
	}

	const maxPublicIDAttempts = 3
	var err error
	for attempt := 1; attempt <= maxPublicIDAttempts; attempt++ {
		var created StatusPage
		created, err = s.createStatusPageAttempt(ctx, sp, monitors)
		if err == nil {
			return created, nil
		}
		// ретраим только коллизию public_id — по имени constraint'а, не по коду
		// 23505: тот же код даёт и PK-нарушение status_page_monitors_pkey (дубль monitor_id).
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.ConstraintName != statusPagePublicIDConstraint {
			return StatusPage{}, err
		}
		// коллизия почти невероятна (12 случайных байт), но генератор её не гарантирует.
	}
	return StatusPage{}, fmt.Errorf("uptime: create status page: public id collision after %d attempts: %w", maxPublicIDAttempts, err)
}

// своя транзакция на попытку: после unique-violation tx испорчена до
// ROLLBACK, повторить в той же tx нельзя — CreateStatusPage начинает заново.
func (s *Service) createStatusPageAttempt(ctx context.Context, sp StatusPage, monitors []StatusPageMonitor) (StatusPage, error) {
	publicID, err := newStatusPagePublicID()
	if err != nil {
		return StatusPage{}, err
	}
	sp.PublicID = publicID

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return StatusPage{}, fmt.Errorf("uptime: create status page: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO status_pages (project_id, public_id, title, description, enabled)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		sp.ProjectID, sp.PublicID, sp.Title, sp.Description, sp.Enabled,
	).Scan(&sp.ID)
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

// public_id неизменяем — Update его не трогает.
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
		UPDATE status_pages SET title=$2, description=$3, enabled=$4
		WHERE id=$1`,
		sp.ID, sp.Title, sp.Description, sp.Enabled)
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

// порядок совпадает со scanStatusPage.
const statusPageColumns = "id, project_id, public_id, title, description, enabled"

func scanStatusPage(row interface{ Scan(dest ...any) error }, sp *StatusPage) error {
	return row.Scan(&sp.ID, &sp.ProjectID, &sp.PublicID, &sp.Title, &sp.Description, &sp.Enabled)
}

// сортировка по title, не по slug — новые страницы slug не задают.
func (s *Service) StatusPagesOf(ctx context.Context, projectID int64) ([]StatusPage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+statusPageColumns+`
		FROM status_pages WHERE project_id = $1 ORDER BY title`, projectID)
	if err != nil {
		return nil, fmt.Errorf("uptime: status pages of: %w", err)
	}
	defer rows.Close()
	var out []StatusPage
	for rows.Next() {
		var sp StatusPage
		if err := scanStatusPage(rows, &sp); err != nil {
			return nil, fmt.Errorf("uptime: status pages of: %w", err)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// disabled и несуществующий public_id оба дают ErrNotFound — выключенную
// страницу нельзя отличить снаружи от той, которой не существует.
func (s *Service) StatusPageByPublicID(ctx context.Context, publicID string) (StatusPage, []StatusPageMonitor, error) {
	var sp StatusPage
	row := s.pool.QueryRow(ctx, `
		SELECT `+statusPageColumns+`
		FROM status_pages WHERE public_id = $1 AND enabled = true`, publicID)
	err := scanStatusPage(row, &sp)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusPage{}, nil, ErrNotFound
	}
	if err != nil {
		return StatusPage{}, nil, fmt.Errorf("uptime: status page by public id: %w", err)
	}

	monitors, err := s.StatusPageMonitors(ctx, sp.ID)
	if err != nil {
		return StatusPage{}, nil, err
	}
	return sp, monitors, nil
}

// только enabled — выключенную по старому адресу не палим, единообразно с ByPublicID.
func (s *Service) StatusPageForRedirect(ctx context.Context, legacySlug string) (string, bool, error) {
	var publicID string
	err := s.pool.QueryRow(ctx, `
		SELECT sp.public_id FROM status_page_redirects r
		JOIN status_pages sp ON sp.id = r.status_page_id
		WHERE r.legacy_slug = $1 AND sp.enabled = true`, legacySlug,
	).Scan(&publicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("uptime: status page redirect: %w", err)
	}
	return publicID, true, nil
}

// не фильтрует по enabled: вызывающий сверяет project_id самой страницы,
// не доверяя project id из формы — так чужая страница просто 404.
func (s *Service) StatusPageByID(ctx context.Context, id int64) (StatusPage, error) {
	var sp StatusPage
	row := s.pool.QueryRow(ctx, `
		SELECT `+statusPageColumns+`
		FROM status_pages WHERE id = $1`, id)
	err := scanStatusPage(row, &sp)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatusPage{}, ErrNotFound
	}
	if err != nil {
		return StatusPage{}, fmt.Errorf("uptime: status page by id: %w", err)
	}
	return sp, nil
}

func (s *Service) StatusPageMonitors(ctx context.Context, statusPageID int64) ([]StatusPageMonitor, error) {
	byPage, err := s.StatusPageMonitorsOf(ctx, []int64{statusPageID})
	if err != nil {
		return nil, err
	}
	return byPage[statusPageID], nil
}

// страница без мониторов в карте отсутствует; порядок внутри страницы —
// по position, как у StatusPageMonitors.
func (s *Service) StatusPageMonitorsOf(ctx context.Context, statusPageIDs []int64) (map[int64][]StatusPageMonitor, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT status_page_id, monitor_id, display_name, position
		FROM status_page_monitors WHERE status_page_id = ANY($1)
		ORDER BY status_page_id, position`, statusPageIDs)
	if err != nil {
		return nil, fmt.Errorf("uptime: status page monitors: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]StatusPageMonitor)
	for rows.Next() {
		var pageID int64
		var m StatusPageMonitor
		if err := rows.Scan(&pageID, &m.MonitorID, &m.DisplayName, &m.Position); err != nil {
			return nil, fmt.Errorf("uptime: status page monitors: %w", err)
		}
		out[pageID] = append(out[pageID], m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("uptime: status page monitors: %w", err)
	}
	return out, nil
}
