package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound — заявки с таким id нет в таблице.
	ErrNotFound = errors.New("export: заявка не найдена")
	// ErrNotDeletable — заявка ещё не досчитана (queued/running) либо уже
	// удалена: удалять можно только терминальные заявки.
	ErrNotDeletable = errors.New("export: заявка ещё выполняется")
)

// Store — очередь заявок на выгрузку поверх таблицы export_jobs.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore создаёт стор поверх пула PostgreSQL.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// jobColumns — общий порядок колонок для Get/ByProject; scanJob разбирает
// строку строго в этом порядке.
const jobColumns = `id, project_id, created_by, kind, format,
	coalesce(scope_issue_id, 0), params, include_pii, status, attempts, last_error,
	rows_written, bytes, truncated, file_ext, claimed_at, created_at, finished_at, expires_at`

// rowScanner — общий интерфейс pgx.Row и pgx.Rows, достаточный для scanJob.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanJob разбирает одну строку export_jobs в Job. params хранится как jsonb
// и разбирается отдельно: Scan не умеет декодировать jsonb сразу в структуру
// с учётом её тегов.
func scanJob(row rowScanner) (Job, error) {
	var j Job
	var kind, format, status string
	var raw []byte
	if err := row.Scan(
		&j.ID, &j.ProjectID, &j.CreatedBy, &kind, &format,
		&j.ScopeIssueID, &raw, &j.IncludePII, &status, &j.Attempts, &j.LastError,
		&j.RowsWritten, &j.Bytes, &j.Truncated, &j.FileExt,
		&j.ClaimedAt, &j.CreatedAt, &j.FinishedAt, &j.ExpiresAt,
	); err != nil {
		return Job{}, err
	}
	j.Kind = Kind(kind)
	j.Format = Format(format)
	j.Status = Status(status)
	if err := json.Unmarshal(raw, &j.Params); err != nil {
		return Job{}, fmt.Errorf("разбор params: %w", err)
	}
	return j, nil
}

// Enqueue ставит заявку в очередь и возвращает id новой строки.
func (s *Store) Enqueue(ctx context.Context, j Job) (int64, error) {
	raw, err := json.Marshal(j.Params)
	if err != nil {
		return 0, fmt.Errorf("export: сериализация params: %w", err)
	}
	// scope_issue_id — NULL для заявок без области (не выгрузка одной
	// группы): ноль в Go должен лечь в NULL, а не в валидный, но
	// бессмысленный внешний id.
	var scope any
	if j.ScopeIssueID != 0 {
		scope = j.ScopeIssueID
	}
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO export_jobs (project_id, created_by, kind, format, scope_issue_id,
		                         params, include_pii, file_ext)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		j.ProjectID, j.CreatedBy, string(j.Kind), string(j.Format), scope,
		raw, j.IncludePII, j.Format.Ext()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("export: постановка заявки: %w", err)
	}
	return id, nil
}

// Get возвращает заявку по id либо ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (Job, error) {
	row := s.pool.QueryRow(ctx, "SELECT "+jobColumns+" FROM export_jobs WHERE id = $1", id)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("export: чтение заявки %d: %w", id, err)
	}
	return j, nil
}

// ByProject возвращает заявки проекта, новые сверху, не больше limit штук —
// страница «Выгрузки» списком без пагинации со сдвигом.
func (s *Store) ByProject(ctx context.Context, projectID int64, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+jobColumns+`
		FROM export_jobs WHERE project_id = $1
		ORDER BY created_at DESC LIMIT $2`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("export: список заявок проекта %d: %w", projectID, err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("export: разбор заявки проекта %d: %w", projectID, err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export: список заявок проекта %d: %w", projectID, err)
	}
	return out, nil
}

// ActiveCounts считает незавершённые заявки одним проходом: два счётчика из
// одной выборки дешевле двух запросов и не расходятся между собой во
// времени. proj — все активные заявки проекта, user — из них принадлежащие
// userID (лимиты «на проект» и «на пользователя» проверяются одновременно).
func (s *Store) ActiveCounts(ctx context.Context, projectID, userID int64) (proj int, user int, err error) {
	err = s.pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE created_by = $2)
		FROM export_jobs
		WHERE project_id = $1 AND status IN ('queued','running')`,
		projectID, userID).Scan(&proj, &user)
	if err != nil {
		return 0, 0, fmt.Errorf("export: подсчёт активных заявок проекта %d: %w", projectID, err)
	}
	return proj, user, nil
}

// Delete сносит только досчитанную заявку: у queued/running в этот момент
// может писаться файл, и удаление строки разъехалось бы с воркером. Тот же
// ErrNotDeletable возвращается и для несуществующего id — с точки зрения
// вызывающего это неразличимо от «удалить нельзя».
func (s *Store) Delete(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM export_jobs WHERE id = $1 AND status IN ('done','failed','expired')`, id)
	if err != nil {
		return fmt.Errorf("export: удаление заявки %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotDeletable
	}
	return nil
}
