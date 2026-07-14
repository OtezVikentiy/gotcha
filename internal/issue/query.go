package issue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	"last_seen":  "issues.last_seen DESC",
	"first_seen": "issues.first_seen DESC",
	"times_seen": "issues.times_seen DESC",
}

const defaultSort = "last_seen"

// periodIntervals — whitelist Filter.Period: в SQL-текст попадает только
// это заранее заданное выражение интервала, никогда пользовательская строка.
// Невалидное значение Period игнорируется (как будто фильтра нет), тот же
// принцип, что и у sortColumns.
var periodIntervals = map[string]string{
	"24h": "24 hours",
	"7d":  "7 days",
	"30d": "30 days",
}

const (
	defaultPerPage = 25
	maxPerPage     = 100
)

// Filter — параметры выборки issue в List.
type Filter struct {
	Status      string // "", unresolved, resolved, ignored
	Level       string // "", debug..fatal
	Query       string // подстрока в title/culprit (ILIKE)
	Sort        string // last_seen (default) | first_seen | times_seen
	Environment string // "" = все окружения; иначе EXISTS по issue_environments
	Period      string // "" = за всё время; 24h | 7d | 30d (whitelist periodIntervals)
	Page        int
	PerPage     int
}

const issueColumns = `id, project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen, assignee_id`

// issueColumnsJoined/issueFromJoined — то же самое, но с квалификацией
// issues. и колонкой assignee_email из LEFT JOIN users (для List/Get,
// которым нужна колонка Assignee). issueColumns (без join) остаётся для
// ActiveSince, которому assignee_email не нужен.
const issueColumnsJoined = `issues.id, issues.project_id, issues.fingerprint, issues.title, issues.culprit, issues.level, issues.status, issues.first_seen, issues.last_seen, issues.times_seen, issues.assignee_id, coalesce(u.email, '') AS assignee_email`
const issueFromJoined = `issues LEFT JOIN users u ON u.id = issues.assignee_id`

func scanIssue(row interface{ Scan(dest ...any) error }, i *Issue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Title, &i.Culprit, &i.Level, &i.Status,
		&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID)
}

func scanIssueWithAssignee(row interface{ Scan(dest ...any) error }, i *Issue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Title, &i.Culprit, &i.Level, &i.Status,
		&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID, &i.AssigneeEmail)
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
	sb.WriteString(issueColumnsJoined)
	sb.WriteString(", count(*) OVER() AS total FROM ")
	sb.WriteString(issueFromJoined)
	sb.WriteString(" WHERE issues.project_id = $1")
	args := []any{projectID}

	if f.Status != "" {
		args = append(args, f.Status)
		fmt.Fprintf(&sb, " AND issues.status = $%d", len(args))
	}
	if f.Level != "" {
		args = append(args, f.Level)
		fmt.Fprintf(&sb, " AND issues.level = $%d", len(args))
	}
	if f.Query != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Query)
		args = append(args, "%"+escaped+"%")
		idx := len(args)
		fmt.Fprintf(&sb, " AND (issues.title ILIKE $%d OR issues.culprit ILIKE $%d)", idx, idx)
	}
	if f.Environment != "" {
		args = append(args, f.Environment)
		fmt.Fprintf(&sb, " AND EXISTS (SELECT 1 FROM issue_environments ie WHERE ie.issue_id = issues.id AND ie.environment = $%d)", len(args))
	}
	if interval, ok := periodIntervals[f.Period]; ok {
		fmt.Fprintf(&sb, " AND issues.last_seen >= now() - interval '%s'", interval)
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
			&i.FirstSeen, &i.LastSeen, &i.TimesSeen, &i.AssigneeID, &i.AssigneeEmail, &total); err != nil {
			return nil, 0, fmt.Errorf("issue: list scan: %w", err)
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	return items, total, nil
}

// ActiveSince возвращает issue проекта, у которых last_seen >= since —
// используется spike-воркером алертинга, чтобы ограничить сканирование окна
// правила только недавно активными issue, а не всеми issue проекта.
func (s *Service) ActiveSince(ctx context.Context, projectID int64, since time.Time) ([]Issue, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND last_seen >= $2 ORDER BY last_seen DESC",
		projectID, since)
	if err != nil {
		return nil, fmt.Errorf("issue: active since: %w", err)
	}
	defer rows.Close()

	var out []Issue
	for rows.Next() {
		var i Issue
		if err := scanIssue(rows, &i); err != nil {
			return nil, fmt.Errorf("issue: active since scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: active since: %w", err)
	}
	return out, nil
}

// Get возвращает issue по id (с AssigneeEmail) или ErrNotFound.
func (s *Service) Get(ctx context.Context, issueID int64) (Issue, error) {
	var i Issue
	row := s.pool.QueryRow(ctx, "SELECT "+issueColumnsJoined+" FROM "+issueFromJoined+" WHERE issues.id = $1", issueID)
	if err := scanIssueWithAssignee(row, &i); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Issue{}, ErrNotFound
		}
		return Issue{}, fmt.Errorf("issue: get: %w", err)
	}
	return i, nil
}

// Environments возвращает отсортированный уникальный список environment,
// в которых видели issue проекта (из денормализованной issue_environments).
func (s *Service) Environments(ctx context.Context, projectID int64) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT DISTINCT environment FROM issue_environments WHERE project_id = $1 ORDER BY environment", projectID)
	if err != nil {
		return nil, fmt.Errorf("issue: environments: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, fmt.Errorf("issue: environments scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: environments: %w", err)
	}
	return out, nil
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
