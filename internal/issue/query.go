package issue

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	ErrNotFound      = errors.New("issue: not found")
	ErrInvalidStatus = errors.New("issue: invalid status")
)

// validStatuses — единственные допустимые значения issues.status
// (совпадают с CHECK-constraint в миграции).
var validStatuses = map[string]bool{
	"unresolved": true,
	"resolved":   true,
	"ignored":    true,
}

// sortColumns — whitelist сортировки: в SQL-текст попадает только
// это заранее заданное выражение, никогда пользовательская строка.
var sortColumns = map[string]string{
	"last_seen":  "last_seen DESC",
	"first_seen": "first_seen DESC",
	"times_seen": "times_seen DESC",
}

const defaultSort = "last_seen"

const (
	defaultPerPage = 25
	maxPerPage     = 100
)

// Filter — параметры выборки issue в List.
type Filter struct {
	Status  string // "", unresolved, resolved, ignored
	Level   string // "", debug..fatal
	Query   string // подстрока в title/culprit (ILIKE)
	Sort    string // last_seen (default) | first_seen | times_seen
	Page    int
	PerPage int
}

const issueColumns = `id, project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen, assignee_id`

func scanIssue(row interface{ Scan(dest ...any) error }, i *Issue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Title, &i.Culprit, &i.Level, &i.Status,
		&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID)
}

// List возвращает страницу issue проекта с фильтрами и total (без учёта пагинации).
func (s *Service) List(ctx context.Context, projectID int64, f Filter) ([]Issue, int64, error) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}

	order, ok := sortColumns[f.Sort]
	if !ok {
		order = sortColumns[defaultSort]
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(issueColumns)
	sb.WriteString(", count(*) OVER() AS total FROM issues WHERE project_id = $1")
	args := []any{projectID}

	if f.Status != "" {
		args = append(args, f.Status)
		fmt.Fprintf(&sb, " AND status = $%d", len(args))
	}
	if f.Level != "" {
		args = append(args, f.Level)
		fmt.Fprintf(&sb, " AND level = $%d", len(args))
	}
	if f.Query != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query)
		args = append(args, "%"+escaped+"%")
		idx := len(args)
		fmt.Fprintf(&sb, " AND (title ILIKE $%d OR culprit ILIKE $%d)", idx, idx)
	}

	sb.WriteString(" ORDER BY ")
	sb.WriteString(order)

	args = append(args, perPage)
	fmt.Fprintf(&sb, " LIMIT $%d", len(args))
	args = append(args, (page-1)*perPage)
	fmt.Fprintf(&sb, " OFFSET $%d", len(args))

	rows, err := s.pool.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	defer rows.Close()

	var items []Issue
	var total int64
	for rows.Next() {
		var i Issue
		if err := rows.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Title, &i.Culprit, &i.Level, &i.Status,
			&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID, &total); err != nil {
			return nil, 0, fmt.Errorf("issue: list scan: %w", err)
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	return items, total, nil
}

// Get возвращает issue по id или ErrNotFound.
func (s *Service) Get(ctx context.Context, issueID int64) (Issue, error) {
	var i Issue
	row := s.pool.QueryRow(ctx, "SELECT "+issueColumns+" FROM issues WHERE id = $1", issueID)
	if err := scanIssue(row, &i); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Issue{}, ErrNotFound
		}
		return Issue{}, fmt.Errorf("issue: get: %w", err)
	}
	return i, nil
}

// SetStatus меняет статус одного issue. Невалидный статус → ErrInvalidStatus,
// отсутствующий issue → ErrNotFound.
func (s *Service) SetStatus(ctx context.Context, issueID int64, status string) error {
	if !validStatuses[status] {
		return ErrInvalidStatus
	}
	ct, err := s.pool.Exec(ctx, "UPDATE issues SET status = $1 WHERE id = $2", status, issueID)
	if err != nil {
		return fmt.Errorf("issue: set status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetStatusBulk меняет статус набора issue, ограниченных проектом projectID;
// id из чужих проектов игнорируются. Возвращает число изменённых строк.
func (s *Service) SetStatusBulk(ctx context.Context, projectID int64, ids []int64, status string) (int64, error) {
	if !validStatuses[status] {
		return 0, ErrInvalidStatus
	}
	ct, err := s.pool.Exec(ctx,
		"UPDATE issues SET status = $1 WHERE project_id = $2 AND id = ANY($3)",
		status, projectID, ids)
	if err != nil {
		return 0, fmt.Errorf("issue: set status bulk: %w", err)
	}
	return ct.RowsAffected(), nil
}

// Assign назначает issue пользователю; userID == nil снимает назначение.
// Несуществующий issue → ErrNotFound.
func (s *Service) Assign(ctx context.Context, issueID int64, userID *int64) error {
	ct, err := s.pool.Exec(ctx, "UPDATE issues SET assignee_id = $1 WHERE id = $2", userID, issueID)
	if err != nil {
		return fmt.Errorf("issue: assign: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
