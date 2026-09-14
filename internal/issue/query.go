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

const (
	StatusUnresolved = "unresolved"
	StatusResolved   = "resolved"
	StatusIgnored    = "ignored"
)

// Совпадают с CHECK-constraint в миграции; порядок — как в дропдауне фильтра.
var Statuses = []string{StatusUnresolved, StatusResolved, StatusIgnored}

var validStatuses = func() map[string]bool {
	m := make(map[string]bool, len(Statuses))
	for _, s := range Statuses {
		m[s] = true
	}
	return m
}()

func IsValidStatus(v string) bool { return validStatuses[v] }

const (
	LevelDebug   = "debug"
	LevelInfo    = "info"
	LevelWarning = "warning"
	LevelError   = "error"
	LevelFatal   = "fatal"
)

// Порядок важен — используется в дропдауне фильтра.
var Levels = []string{LevelDebug, LevelInfo, LevelWarning, LevelError, LevelFatal}

var validLevels = func() map[string]bool {
	m := make(map[string]bool, len(Levels))
	for _, l := range Levels {
		m[l] = true
	}
	return m
}()

func IsValidLevel(v string) bool { return validLevels[v] }

// Whitelist: в SQL-текст попадает только заранее заданное выражение, никогда
// пользовательская строка.
var sortColumns = map[string]string{
	"last_seen":  "issues.last_seen DESC",
	"first_seen": "issues.first_seen DESC",
	"times_seen": "issues.times_seen DESC",
}

const defaultSort = "last_seen"

const (
	DefaultPerPage = 25
	maxPerPage     = 100
)

type Filter struct {
	Status      string // "", unresolved, resolved, ignored
	Level       string // "", debug..fatal
	Query       string // подстрока в title/culprit (ILIKE)
	Sort        string // last_seen (default) | first_seen | times_seen
	Environment string // "" = все окружения; иначе EXISTS по issue_environments
	// Since/Until — полуинтервал [Since, Until) по last_seen, нулевое значение = без
	// границы; Until исключающая — иначе соседние дневные окна делят полночь.
	Since   time.Time
	Until   time.Time
	Page    int
	PerPage int
}

const issueColumns = `id, project_id, fingerprint, title, culprit, level, status, first_seen, last_seen, times_seen, assignee_id`

// С квалификацией issues. и колонкой assignee_email из LEFT JOIN users — для
// List/Get. issueColumns (без join) — для запросов, которым Assignee не нужен.
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

// Общий код для List/StreamForExport/IDsForFilter: один и тот же набор
// предикатов обязан ограничивать все запросы одинаково.
func buildIssueFilter(projectID int64, f Filter) (string, []any) {
	var sb strings.Builder
	sb.WriteString("issues.project_id = $1")
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
	if !f.Since.IsZero() {
		args = append(args, f.Since)
		fmt.Fprintf(&sb, " AND issues.last_seen >= $%d", len(args))
	}
	if !f.Until.IsZero() {
		args = append(args, f.Until)
		fmt.Fprintf(&sb, " AND issues.last_seen < $%d", len(args))
	}
	return sb.String(), args
}

// total считается отдельным лёгким count(*), а не count(*) OVER(): без
// PARTITION BY он материализует и сортирует ВСЕ строки до LIMIT/OFFSET.
func (s *Service) List(ctx context.Context, projectID int64, f Filter) ([]Issue, int64, error) {
	page := f.Page
	if page < 1 {
		page = 1
	}
	perPage := f.PerPage
	if perPage <= 0 {
		perPage = DefaultPerPage
	}
	if perPage > maxPerPage {
		perPage = maxPerPage
	}

	order, ok := sortColumns[f.Sort]
	if !ok {
		order = sortColumns[defaultSort]
	}

	where, args := buildIssueFilter(projectID, f)
	offset := (page - 1) * perPage

	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM issues WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("issue: list count: %w", err)
	}
	if int64(offset) >= total {
		// total=0 здесь воспроизведён явно: шаблон (issues.templ, pagerPrev)
		// трактует total<=0 как «страницы нет, веди на первую».
		return nil, 0, nil
	}

	var sb strings.Builder
	sb.WriteString("SELECT ")
	sb.WriteString(issueColumnsJoined)
	sb.WriteString(" FROM ")
	sb.WriteString(issueFromJoined)
	sb.WriteString(" WHERE ")
	sb.WriteString(where)
	sb.WriteString(" ORDER BY ")
	sb.WriteString(order)

	rowArgs := append(append([]any{}, args...), perPage, offset)
	fmt.Fprintf(&sb, " LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)

	rows, err := s.pool.Query(ctx, sb.String(), rowArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	defer rows.Close()

	var items []Issue
	for rows.Next() {
		var i Issue
		if err := scanIssueWithAssignee(rows, &i); err != nil {
			return nil, 0, fmt.Errorf("issue: list scan: %w", err)
		}
		items = append(items, i)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("issue: list: %w", err)
	}
	return items, total, nil
}

const exportPageSize = 500

// Взят РАВНЫМ eventStreamSafetyLimit: MaxRows любой заявки уже гарантированно
// меньше него, снимок в норме не упирается в этот потолок первым.
const issueExportSnapshotSafetyLimit = 1_000_000

// Отказ, а не тихая обрезка снимка: обрезанный по этой границе снимок
// выглядел бы как «выгрузка полна», хотя часть групп в неё не вошла бы.
var ErrExportSnapshotTooLarge = errors.New("issue: export snapshot exceeds safety limit")

// Снимок id резолвится ОДНИМ запросом ДО обхода — мутация last_seen группы
// во время обхода её не теряет и не задваивает; созданные после снимка не попадают.
func (s *Service) StreamForExport(ctx context.Context, projectID int64, f Filter, fn func(Issue) error) error {
	return s.streamForExport(ctx, projectID, f, issueExportSnapshotSafetyLimit, fn)
}

// Потолок снимка — параметр, а не жёстко зашитая issueExportSnapshotSafetyLimit:
// тест на переполнение иначе стоил бы вставки 1 000 001 строки на каждый прогон.
func (s *Service) streamForExport(ctx context.Context, projectID int64, f Filter, snapshotLimit int, fn func(Issue) error) error {
	where, args := buildIssueFilter(projectID, f)

	snapQ := fmt.Sprintf("SELECT issues.id FROM issues WHERE %s ORDER BY issues.last_seen DESC, issues.id DESC LIMIT %d",
		where, snapshotLimit+1)
	rows, err := s.pool.Query(ctx, snapQ, args...)
	if err != nil {
		return fmt.Errorf("issue: stream for export snapshot: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("issue: stream for export snapshot scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("issue: stream for export snapshot: %w", err)
	}
	if len(ids) > snapshotLimit {
		return ErrExportSnapshotTooLarge
	}

	for start := 0; start < len(ids); start += exportPageSize {
		end := start + exportPageSize
		if end > len(ids) {
			end = len(ids)
		}
		page := ids[start:end]

		// project_id в условии избыточен (id уже из снимка того же projectID),
		// но полагаться на это дороже одного лишнего параметра.
		q := "SELECT " + issueColumnsJoined + " FROM " + issueFromJoined + " WHERE issues.project_id = $1 AND issues.id = ANY($2)"
		pageRows, err := s.pool.Query(ctx, q, projectID, page)
		if err != nil {
			return fmt.Errorf("issue: stream for export page: %w", err)
		}
		byID := make(map[int64]Issue, len(page))
		for pageRows.Next() {
			var it Issue
			if err := scanIssueWithAssignee(pageRows, &it); err != nil {
				pageRows.Close()
				return fmt.Errorf("issue: stream for export scan: %w", err)
			}
			byID[it.ID] = it
		}
		pageRows.Close()
		if err := pageRows.Err(); err != nil {
			return fmt.Errorf("issue: stream for export page: %w", err)
		}

		// Порядок отдачи — порядок снимка (page), Postgres его для id = ANY не гарантирует.
		var batchIDs []int64
		for _, id := range page {
			if _, ok := byID[id]; ok {
				batchIDs = append(batchIDs, id)
			}
		}
		if len(batchIDs) > 0 {
			envs, err := s.environmentsForIssues(ctx, batchIDs)
			if err != nil {
				return err
			}
			for _, id := range batchIDs {
				it := byID[id]
				it.Environments = envs[id]
				byID[id] = it
			}
		}

		for _, id := range batchIDs {
			if err := fn(byID[id]); err != nil {
				return err
			}
		}
	}
	return nil
}

// overflow=true — резолвится больше limit групп: обрезать список молча
// нельзя, вызывающая сторона обязана вернуть отказ, а не отдать неполный список.
func (s *Service) IDsForFilter(ctx context.Context, projectID int64, f Filter, limit int) ([]int64, bool, error) {
	where, args := buildIssueFilter(projectID, f)
	q := fmt.Sprintf("SELECT issues.id FROM issues WHERE %s ORDER BY issues.last_seen DESC, issues.id DESC LIMIT %d",
		where, limit+1)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("issue: ids for filter: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("issue: ids for filter scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("issue: ids for filter: %w", err)
	}

	if len(ids) > limit {
		return ids[:limit], true, nil
	}
	return ids, false, nil
}

// Одним запросом на весь набор issue вместо N+1 похода в issue_environments по каждой странице.
func (s *Service) environmentsForIssues(ctx context.Context, ids []int64) (map[int64][]string, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT issue_id, environment FROM issue_environments WHERE issue_id = ANY($1) ORDER BY issue_id, environment", ids)
	if err != nil {
		return nil, fmt.Errorf("issue: environments for issues: %w", err)
	}
	defer rows.Close()

	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var env string
		if err := rows.Scan(&id, &env); err != nil {
			return nil, fmt.Errorf("issue: environments for issues scan: %w", err)
		}
		out[id] = append(out[id], env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: environments for issues: %w", err)
	}
	return out, nil
}

// Используется spike-воркером алертинга, чтобы сканировать окно правила
// только по недавно активным issue, а не по всем issue проекта.
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

// В отличие от ActiveSince (та фильтрует по last_seen — «недавно шумевшие»),
// здесь first_seen — «новые проблемы», давно заведённые issue не считаются.
func (s *Service) CountNewSince(ctx context.Context, projectID int64, since time.Time) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM issues WHERE project_id = $1 AND first_seen >= $2",
		projectID, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("issue: count new since: %w", err)
	}
	return n, nil
}

// Фильтр по project_id обязателен: идентификаторы приходят из ответа
// ClickHouse, и доверять им как «уже проверенным» нельзя.
func (s *Service) ByIDs(ctx context.Context, projectID int64, ids []int64) ([]Issue, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		"SELECT "+issueColumns+" FROM issues WHERE project_id = $1 AND id = ANY($2) ORDER BY last_seen DESC",
		projectID, ids)
	if err != nil {
		return nil, fmt.Errorf("issue: by ids: %w", err)
	}
	defer rows.Close()

	var out []Issue
	for rows.Next() {
		var i Issue
		if err := scanIssue(rows, &i); err != nil {
			return nil, fmt.Errorf("issue: by ids scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("issue: by ids: %w", err)
	}
	return out, nil
}

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

func (s *Service) Exists(ctx context.Context, projectID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM issues WHERE project_id = $1)", projectID).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

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
