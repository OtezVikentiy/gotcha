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

// Уровни issue. В отличие от status, колонка issues.level не ограничена
// CHECK в схеме (см. миграцию 0003) — единственный источник истины для
// множества значений раньше был разбросан: приём (internal/ingest) держал
// свою копию списка для валидации, веб-слой — свою для рендера бейджа и
// дропдауна фильтра. Экспортированные константы делают issue владельцем
// набора: приём и веб теперь ссылаются на них вместо повторения строк.
const (
	LevelDebug   = "debug"
	LevelInfo    = "info"
	LevelWarning = "warning"
	LevelError   = "error"
	LevelFatal   = "fatal"
)

// Levels — все допустимые уровни issue, по возрастанию серьёзности. Порядок
// важен: это же он используется в дропдауне фильтра.
var Levels = []string{LevelDebug, LevelInfo, LevelWarning, LevelError, LevelFatal}

// sortColumns — whitelist сортировки: в SQL-текст попадает только
// это заранее заданное выражение, никогда пользовательская строка.
var sortColumns = map[string]string{
	"last_seen":  "issues.last_seen DESC",
	"first_seen": "issues.first_seen DESC",
	"times_seen": "issues.times_seen DESC",
}

const defaultSort = "last_seen"

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
	// Since/Until — границы окна по last_seen; нулевое значение = без границы.
	//
	// Раньше здесь была строка периода из белого списка (24h|7d|30d), и список
	// проблем поэтому имел свой, более бедный фильтр времени: ни часа, ни
	// произвольного диапазона — при том что на соседних страницах общий контрол
	// умел и то, и другое. Границы приходят параметрами запроса, поэтому любое
	// окно, которое умеет разобрать веб-слой, работает и здесь.
	Since   time.Time
	Until   time.Time
	Page    int
	PerPage int
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

// buildIssueFilter собирает WHERE-условие и позиционные аргументы фильтра —
// общую часть для запроса строк и отдельного запроса total (см. List): один
// и тот же набор предикатов должен ограничивать оба запроса одинаково,
// иначе total и фактически показанный список могли бы разойтись.
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
		fmt.Fprintf(&sb, " AND issues.last_seen <= $%d", len(args))
	}
	return sb.String(), args
}

// List возвращает страницу issue проекта с фильтрами и total (без учёта пагинации).
//
// total считается отдельным запросом без JOIN/ORDER BY, а не count(*) OVER()
// в основном запросе. OVER() без PARTITION BY заставляет планировщик
// материализовать, отсортировать и обсчитать ВСЕ подходящие строки прежде
// чем применить LIMIT/OFFSET — при большом числе issue страница 1 стоила бы
// как чтение всего набора, а не 25 строк. Веб-слой показывает total как
// точное число страниц пагинатора («{page} / {totalPages}»,
// internal/web/templates/issues.templ) — то есть точное значение действительно
// нужно, а не просто признак «есть ли ещё одна страница», и убрать его
// вовсе нельзя без изменения интерфейса. Отдельный лёгкий count(*) даёт то
// же число для пагинатора, но не мешает планировщику использовать индекс по
// колонке сортировки и остановиться после LIMIT строк при выборке самих issue.
//
// Счётчик и строки — теперь два отдельных обращения к БД, а не один снимок:
// между ними приём может записать новую issue, и total разойдётся со
// списком на единицу-две на границе страницы. Раньше такого расхождения не
// было вовсе (один запрос — один снимок). Это осознанный размен: цена —
// временная неточность счётчика страниц, самоисправляющаяся при следующем
// открытии списка; цена бездействия — полный скан на каждое открытие.
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

	where, args := buildIssueFilter(projectID, f)
	offset := (page - 1) * perPage

	var total int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM issues WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("issue: list count: %w", err)
	}
	if int64(offset) >= total {
		// Покрывает и total==0 (offset>=0 всегда), и страницу за пределами
		// данных (?page=<огромное> или фильтр применён между запросами) —
		// отдельной проверки на total==0 не нужно, offset неотрицателен.
		//
		// Раньше count(*) OVER() сидел в ТОМ ЖЕ запросе, что и LIMIT/OFFSET —
		// на пустой странице клиент не получал ни одной строки и,
		// соответственно, ни одного значения total, поэтому total оставался
		// нулём (zero value). Шаблон (issues.templ, pagerPrev) на это
		// опирается буквально: total<=0 читается как «страницы нет, веди на
		// первую», а не как «страница X из total». Раз total теперь считается
		// отдельным запросом ДО пагинации, он был бы больше нуля и на
		// out-of-range странице — это изменило бы поведение пагинатора,
		// поэтому здесь тот же нулевой total воспроизведён явно.
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
// ByIDs возвращает группы проекта по списку идентификаторов.
//
// Существует ради spike-детектора: ему нужны заголовки только тех групп, что
// перешагнули порог, а не всех активных. Раньше он забирал из PostgreSQL все
// активные группы проекта, чтобы затем отбросить почти все.
//
// Фильтр по project_id обязателен и здесь: идентификаторы приходят из ответа
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

// Exists — есть ли у проекта хоть одна проблема. Для онбординг-галочки нужен
// только факт наличия, поэтому EXISTS вместо List(Filter{}): последний из-за
// count(*) OVER() материализует весь набор проблем проекта на каждый рендер.
func (s *Service) Exists(ctx context.Context, projectID int64) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM issues WHERE project_id = $1)", projectID).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
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
