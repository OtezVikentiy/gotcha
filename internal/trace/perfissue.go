package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound      = errors.New("trace: perf issue not found")
	ErrInvalidStatus = errors.New("trace: invalid perf issue status")
)

// должны совпадать с CHECK-constraint perf_issues.status в БД.
var validStatuses = map[string]bool{
	"unresolved": true,
	"resolved":   true,
	"ignored":    true,
}

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

const perfSampleTTL = time.Hour

type PerfIssue struct {
	ID        int64
	ProjectID int64

	Fingerprint string
	Kind        string // KindNPlusOne | KindSlowDBQuery | KindHTTPFlood
	// пусто у http_flood — параметром служит Culprit; для остальных kind это нормализованный запрос.
	Description string
	// новые строки всегда пишут пустым — это fallback для строк из старых миграций.
	Title   string
	Culprit string
	Status  string // unresolved | resolved | ignored

	Count     int64
	FirstSeen time.Time
	LastSeen  time.Time

	SampleTraceID string          // трейс последнего обнаружения — с него открывают waterfall
	Evidence      json.RawMessage // count/total_ms/span_ids и поля, специфичные для вида
}

type IssueService struct {
	pool *pgxpool.Pool
}

func NewIssueService(pool *pgxpool.Pool) *IssueService {
	return &IssueService{pool: pool}
}

const perfIssueColumns = `id, project_id, fingerprint, kind, description, title, culprit, status,
	count, first_seen, last_seen, sample_trace_id, evidence`

func scanPerfIssue(row interface{ Scan(dest ...any) error }, i *PerfIssue) error {
	return row.Scan(&i.ID, &i.ProjectID, &i.Fingerprint, &i.Kind, &i.Description, &i.Title, &i.Culprit, &i.Status,
		&i.Count, &i.FirstSeen, &i.LastSeen, &i.SampleTraceID, &i.Evidence)
}

type RecordResult struct {
	Issue      PerfIssue
	Created    bool // строка ФИЗИЧЕСКИ вставлена этим вызовом
	Regression bool // проблема была resolved и снова обнаружена
	Suppressed bool // строки нет: часовой кап на СОЗДАНИЕ проблем выбран (Issue пустой)
}

const MaxNewPerfIssuesPerHour = 100

// окно прыгающее (tumbling), не скользящее.
const perfIssueWindow = time.Hour

func (s *IssueService) Record(ctx context.Context, projectID int64, f Finding, traceID string) (RecordResult, error) {
	evidence, err := json.Marshal(f.Evidence)
	if err != nil || f.Evidence == nil {
		// Нечитаемое evidence не повод терять находку: пишем пустой объект.
		evidence = []byte(`{}`)
	}

	sampleCutoff := time.Now().Add(-perfSampleTTL)

	// сначала пробуем обновить существующую строку — часовой кап тратится
	// только на реально новые проблемы, а не на каждое повторное обнаружение.
	res, found, err := s.touch(ctx, projectID, f, evidence, traceID, sampleCutoff)
	if err != nil {
		return RecordResult{}, err
	}
	if found {
		return res, nil
	}

	claimed, suppressed, err := s.claimNewIssue(ctx, projectID)
	if err != nil {
		return RecordResult{}, err
	}
	if !claimed {
		// Молчаливый кап читался бы как «мы нашли всё»: сколько находок осталось
		// без строки — в лог.
		slog.Warn("perf issue creation capped, finding recorded nowhere",
			"project_id", projectID, "kind", f.Kind, "culprit", f.Culprit,
			"fingerprint", f.Fingerprint, "limit_per_hour", MaxNewPerfIssuesPerHour,
			"suppressed_in_window", suppressed)
		return RecordResult{Suppressed: true}, nil
	}
	return s.create(ctx, projectID, f, evidence, traceID)
}

// ignored не двигает last_seen (List сортирует по нему); evidence и
// sample_trace_id обновляются только вместе, не чаще perfSampleTTL.
func (s *IssueService) touch(ctx context.Context, projectID int64, f Finding,
	evidence []byte, traceID string, sampleCutoff time.Time) (RecordResult, bool, error) {

	const q = `
WITH old AS (
    SELECT status FROM perf_issues WHERE project_id = $1 AND fingerprint = $2
), up AS (
    UPDATE perf_issues SET
        kind            = $3,
        description     = $4,
        culprit         = $5,
        count           = perf_issues.count + 1,
        last_seen       = CASE WHEN status = 'ignored' THEN last_seen ELSE now() END,
        sample_trace_id = CASE WHEN sample_at <= $8 THEN $6 ELSE sample_trace_id END,
        evidence        = CASE WHEN sample_at <= $8 THEN $7::jsonb ELSE evidence END,
        sample_at       = CASE WHEN sample_at <= $8 THEN now() ELSE sample_at END,
        status          = CASE WHEN status = 'resolved' THEN 'unresolved' ELSE status END
    WHERE project_id = $1 AND fingerprint = $2
    RETURNING ` + perfIssueColumns + `
)
SELECT up.id, up.project_id, up.fingerprint, up.kind, up.description, up.title, up.culprit, up.status,
       up.count, up.first_seen, up.last_seen, up.sample_trace_id, up.evidence,
       coalesce(old.status = 'resolved', false) AS regression
FROM up LEFT JOIN old ON true`

	var r RecordResult
	row := s.pool.QueryRow(ctx, q,
		projectID, f.Fingerprint, f.Kind, f.Description, f.Culprit, traceID, evidence, sampleCutoff)
	if err := row.Scan(&r.Issue.ID, &r.Issue.ProjectID, &r.Issue.Fingerprint, &r.Issue.Kind, &r.Issue.Description,
		&r.Issue.Title, &r.Issue.Culprit, &r.Issue.Status, &r.Issue.Count, &r.Issue.FirstSeen, &r.Issue.LastSeen,
		&r.Issue.SampleTraceID, &r.Issue.Evidence, &r.Regression); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return RecordResult{}, false, nil // проблемы ещё нет
		}
		return RecordResult{}, false, fmt.Errorf("trace: record perf issue: %w", err)
	}
	return r, true, nil
}

// ON CONFLICT ловит гонку двух воркеров на один слот; created — из xmax=0, не снимка old.
// снимок old соврал бы при гонке; regression же считается по old — та гонка безобиднее.
func (s *IssueService) create(ctx context.Context, projectID int64, f Finding,
	evidence []byte, traceID string) (RecordResult, error) {

	const q = `
WITH old AS (
    SELECT status FROM perf_issues WHERE project_id = $1 AND fingerprint = $2
), up AS (
    INSERT INTO perf_issues (project_id, fingerprint, kind, description, culprit, count, sample_trace_id, evidence)
    VALUES ($1, $2, $3, $4, $5, 1, $6, $7)
    ON CONFLICT (project_id, fingerprint) DO UPDATE SET
        kind        = EXCLUDED.kind,
        description = EXCLUDED.description,
        culprit     = EXCLUDED.culprit,
        count     = perf_issues.count + 1,
        last_seen = CASE WHEN perf_issues.status = 'ignored'
                         THEN perf_issues.last_seen ELSE now() END,
        status    = CASE WHEN perf_issues.status = 'resolved' THEN 'unresolved' ELSE perf_issues.status END
    RETURNING ` + perfIssueColumns + `, (xmax = 0) AS created
)
SELECT up.id, up.project_id, up.fingerprint, up.kind, up.description, up.title, up.culprit, up.status,
       up.count, up.first_seen, up.last_seen, up.sample_trace_id, up.evidence,
       up.created,
       (NOT up.created AND coalesce(old.status = 'resolved', false)) AS regression
FROM up LEFT JOIN old ON true`

	var r RecordResult
	row := s.pool.QueryRow(ctx, q,
		projectID, f.Fingerprint, f.Kind, f.Description, f.Culprit, traceID, evidence)
	if err := row.Scan(&r.Issue.ID, &r.Issue.ProjectID, &r.Issue.Fingerprint, &r.Issue.Kind, &r.Issue.Description,
		&r.Issue.Title, &r.Issue.Culprit, &r.Issue.Status, &r.Issue.Count, &r.Issue.FirstSeen, &r.Issue.LastSeen,
		&r.Issue.SampleTraceID, &r.Issue.Evidence, &r.Created, &r.Regression); err != nil {
		return RecordResult{}, fmt.Errorf("trace: record perf issue: %w", err)
	}
	return r, nil
}

// claimed=false — лимит окна выбран; suppressed растёт без капа, это просто
// счётчик подавленного за окно для лога.
func (s *IssueService) claimNewIssue(ctx context.Context, projectID int64) (claimed bool, suppressed int, err error) {
	cutoff := time.Now().Add(-perfIssueWindow)
	var created int
	err = s.pool.QueryRow(ctx, `
		INSERT INTO perf_issue_throttle (project_id, window_start, created, suppressed)
		VALUES ($1, now(), 1, 0)
		ON CONFLICT (project_id) DO UPDATE SET
			window_start = CASE WHEN perf_issue_throttle.window_start <= $2
			                    THEN now() ELSE perf_issue_throttle.window_start END,
			created      = CASE WHEN perf_issue_throttle.window_start <= $2
			                    THEN 1 ELSE perf_issue_throttle.created + 1 END,
			suppressed   = CASE WHEN perf_issue_throttle.window_start <= $2
			                    THEN 0 ELSE perf_issue_throttle.suppressed END
		WHERE perf_issue_throttle.window_start <= $2 OR perf_issue_throttle.created < $3
		RETURNING created`,
		projectID, cutoff, MaxNewPerfIssuesPerHour).Scan(&created)
	if err == nil {
		return true, 0, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, 0, fmt.Errorf("trace: claim new perf issue: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		UPDATE perf_issue_throttle SET suppressed = suppressed + 1
		WHERE project_id = $1 RETURNING suppressed`, projectID).Scan(&suppressed); err != nil {
		return false, 0, fmt.Errorf("trace: mark perf issue suppressed: %w", err)
	}
	return false, suppressed, nil
}

// невалидный status → ErrInvalidStatus, иначе фильтр тихо вернул бы пустой список.
func (s *IssueService) List(ctx context.Context, projectID int64, status string, limit int) ([]PerfIssue, error) {
	if status != "" && !validStatuses[status] {
		return nil, ErrInvalidStatus
	}
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	// $2 = '' означает «без фильтра по статусу» — одна форма запроса вместо
	// склейки SQL-строки.
	rows, err := s.pool.Query(ctx, `SELECT `+perfIssueColumns+`
		FROM perf_issues
		WHERE project_id = $1 AND ($2 = '' OR status = $2)
		ORDER BY last_seen DESC
		LIMIT $3`, projectID, status, limit)
	if err != nil {
		return nil, fmt.Errorf("trace: list perf issues: %w", err)
	}
	defer rows.Close()

	var out []PerfIssue
	for rows.Next() {
		var i PerfIssue
		if err := scanPerfIssue(rows, &i); err != nil {
			return nil, fmt.Errorf("trace: list perf issues scan: %w", err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: list perf issues: %w", err)
	}
	return out, nil
}

// projectID в WHERE обязателен: без него угаданный id отдавал бы чужую
// проблему (IDOR). Чужая и несуществующая проблема неотличимы — обе ErrNotFound.
func (s *IssueService) Get(ctx context.Context, projectID, id int64) (PerfIssue, error) {
	var i PerfIssue
	row := s.pool.QueryRow(ctx,
		"SELECT "+perfIssueColumns+" FROM perf_issues WHERE project_id = $1 AND id = $2", projectID, id)
	if err := scanPerfIssue(row, &i); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PerfIssue{}, ErrNotFound
		}
		return PerfIssue{}, fmt.Errorf("trace: get perf issue: %w", err)
	}
	return i, nil
}

// нужен, чтобы проверить доступ к проекту ДО чтения данных — Get/SetStatus уже
// требуют projectID и не годятся для этого первого шага.
func (s *IssueService) ProjectOf(ctx context.Context, id int64) (projectID int64, found bool, err error) {
	err = s.pool.QueryRow(ctx, "SELECT project_id FROM perf_issues WHERE id = $1", id).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("trace: perf issue project: %w", err)
	}
	return projectID, true, nil
}

// невалидный статус → ErrInvalidStatus; отсутствующая или чужая (IDOR) проблема
// — неотличимо ErrNotFound.
func (s *IssueService) SetStatus(ctx context.Context, projectID, id int64, status string) error {
	if !validStatuses[status] {
		return ErrInvalidStatus
	}
	ct, err := s.pool.Exec(ctx,
		"UPDATE perf_issues SET status = $1 WHERE project_id = $2 AND id = $3", status, projectID, id)
	if err != nil {
		return fmt.Errorf("trace: set perf issue status: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
