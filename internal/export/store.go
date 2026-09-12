package export

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// Строго больше jobTimeout воркера, иначе второй инстанс переклеймит заявку, которую пишет первый.
	leaseTTL = 20 * time.Minute
	// После стольких провалов подряд — вероятно системный сбой; дальше заявку добивает SweepStale.
	maxAttempts = 3
	// Один проход на весь хвост держал бы блокировку на таблице, куда идёт приём новых заявок.
	janitorBatchSize = 500
	// Двухаргументная форма pg_advisory_xact_lock — свой keyspace, отдельный от формы воркера/джанитора:
	// пересечение исключено структурно, а не диапазоном id.
	enqueueLockClassProject = 1
	enqueueLockClassUser    = 2
)

var (
	ErrNotFound = errors.New("export: заявка не найдена")
	// Возвращается и для queued/running, и для уже удалённой — оба случая неразличимы для вызывающего.
	ErrNotDeletable = errors.New("export: заявка ещё выполняется")
	// Заявку успели переклеймить или уже финализировал другой вызов Done/Fail.
	// Получивший эту ошибку обязан молча остановиться, не дописывать поверх чужой попытки.
	ErrStaleClaim         = errors.New("export: заявка перехвачена другой попыткой")
	ErrActiveLimitReached = errors.New("export: лимит активных заявок исчерпан")
)

type Store struct {
	pool *pgxpool.Pool
	// Читать только через batch(): нулевое/отрицательное значение — сигнал стора не из NewStore.
	batchSize int
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, batchSize: janitorBatchSize} }

// batchSize<=0 — стор собран в обход NewStore: без защиты LIMIT 0 зациклил бы PurgeRows
// (0 удалённых никогда не меньше нуля) и остановил бы DueForExpiry.
func (s *Store) batch() int {
	if s.batchSize <= 0 {
		return janitorBatchSize
	}
	return s.batchSize
}

// Порядок совпадает со scanJob — менять только вместе.
const jobColumns = `id, project_id, created_by, kind, format,
	coalesce(scope_issue_id, 0), params, include_pii, status, attempts, last_error,
	rows_written, bytes, truncated, file_ext, claimed_at, created_at, finished_at, expires_at,
	failure_reason_key`

type rowScanner interface {
	Scan(dest ...any) error
}

// params — jsonb, Scan не умеет декодировать в структуру с тегами напрямую, разбирается отдельно.
func scanJob(row rowScanner) (Job, error) {
	var j Job
	var kind, format, status string
	var raw []byte
	var reasonKey sql.NullString
	if err := row.Scan(
		&j.ID, &j.ProjectID, &j.CreatedBy, &kind, &format,
		&j.ScopeIssueID, &raw, &j.IncludePII, &status, &j.Attempts, &j.LastError,
		&j.RowsWritten, &j.Bytes, &j.Truncated, &j.FileExt,
		&j.ClaimedAt, &j.CreatedAt, &j.FinishedAt, &j.ExpiresAt,
		&reasonKey,
	); err != nil {
		return Job{}, err
	}
	j.Kind = Kind(kind)
	j.Format = Format(format)
	j.Status = Status(status)
	j.FailureReasonKey = reasonKey.String // NULL → "" — не терминальна либо старше колонки
	if err := json.Unmarshal(raw, &j.Params); err != nil {
		return Job{}, fmt.Errorf("разбор params: %w", err)
	}
	return j, nil
}

// Enqueue работает через пул, EnqueueLimited — через свою транзакцию.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Общая точка вставки для Enqueue/EnqueueLimited — чтобы форматы строки не разошлись.
func insertJob(ctx context.Context, q queryRower, j Job) (int64, error) {
	raw, err := json.Marshal(j.Params)
	if err != nil {
		return 0, fmt.Errorf("export: сериализация params: %w", err)
	}
	// scope_issue_id: 0 в Go — NULL в базе, не валидный внешний id.
	var scope any
	if j.ScopeIssueID != 0 {
		scope = j.ScopeIssueID
	}
	var id int64
	err = q.QueryRow(ctx, `
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

func (s *Store) Enqueue(ctx context.Context, j Job) (int64, error) {
	return insertJob(ctx, s.pool, j)
}

// Транзакции мало — SELECT count(*) не блокирует конкурентный INSERT, нужен advisory-лок до подсчёта.
// Лок user не включает project_id: лимит на пользователя действует по всем его проектам сразу.
func (s *Store) EnqueueLimited(ctx context.Context, j Job, userLimit, projectLimit int) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("export: постановка заявки: begin: %w", err)
	}
	defer tx.Rollback(ctx) // no-op после успешного Commit

	// Порядок фиксирован: user, затем project — иначе дедлок между конкурентными постановками.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2))",
		enqueueLockClassUser, strconv.FormatInt(j.CreatedBy, 10)); err != nil {
		return 0, fmt.Errorf("export: постановка заявки: advisory lock (user): %w", err)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext($2))",
		enqueueLockClassProject, strconv.FormatInt(j.ProjectID, 10)); err != nil {
		return 0, fmt.Errorf("export: постановка заявки: advisory lock (project): %w", err)
	}

	// hashtext: objid — int4, id — bigint; id идёт строкой (strconv), не ::text в SQL —
	// так pgx кодирует int64 корректно.
	var proj, user int
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM export_jobs WHERE project_id = $1 AND status IN ('queued','running')),
			(SELECT count(*) FROM export_jobs WHERE created_by = $2 AND status IN ('queued','running'))`,
		j.ProjectID, j.CreatedBy).Scan(&proj, &user); err != nil {
		return 0, fmt.Errorf("export: постановка заявки: подсчёт активных: %w", err)
	}
	if proj >= projectLimit || user >= userLimit {
		return 0, ErrActiveLimitReached
	}

	id, err := insertJob(ctx, tx, j)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("export: постановка заявки: commit: %w", err)
	}
	return id, nil
}

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

// limit не подменяется: 0 — валидный LIMIT 0, отрицательное — ошибка PostgreSQL.
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

// Фильтр по автору — в SQL, не пост-фильтром: иначе limit съедался бы чужими строками раньше своих.
func (s *Store) ByProjectForUser(ctx context.Context, projectID, uid int64, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+jobColumns+`
		FROM export_jobs WHERE project_id = $1 AND created_by = $2
		ORDER BY created_at DESC LIMIT $3`, projectID, uid, limit)
	if err != nil {
		return nil, fmt.Errorf("export: список заявок проекта %d автора %d: %w", projectID, uid, err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("export: разбор заявки проекта %d автора %d: %w", projectID, uid, err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export: список заявок проекта %d автора %d: %w", projectID, uid, err)
	}
	return out, nil
}

// FOR UPDATE SKIP LOCKED внутри одного UPDATE — атомарно: два клейма не заберут одну строку.
func (s *Store) Claim(ctx context.Context) (Job, bool, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE export_jobs SET status = 'running', attempts = attempts + 1, claimed_at = now()
		WHERE id = (
			SELECT id FROM export_jobs
			WHERE status = 'queued'
			   OR (status = 'running' AND claimed_at < now() - $1::interval AND attempts < $2)
			ORDER BY created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+jobColumns, leaseTTL.String(), maxAttempts)
	j, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("export: клейм заявки: %w", err)
	}
	return j, true, nil
}

// Без этого зависшая заявка висела бы «выполняется» вечно — Claim её не тронет.
// Возвращает заявки, не число: воркер шлёт notifyFailed по каждой из них.
func (s *Store) SweepStale(ctx context.Context) ([]Job, error) {
	// reasonInternal всегда: причина исходного провала неизвестна — лиза протухла вместе с процессом.
	rows, err := s.pool.Query(ctx, `
		UPDATE export_jobs
		SET status = 'failed', finished_at = now(),
		    last_error = 'сборка выгрузки не завершилась за отведённое число попыток',
		    failure_reason_key = $3
		WHERE status = 'running' AND claimed_at < now() - $1::interval AND attempts >= $2
		RETURNING `+jobColumns,
		leaseTTL.String(), maxAttempts, reasonInternal)
	if err != nil {
		return nil, fmt.Errorf("export: снятие зависших заявок: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("export: разбор зависшей заявки: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export: снятие зависших заявок: %w", err)
	}
	return out, nil
}

// attempt фенсит владение попыткой: 0 затронутых строк — сигнал ErrStaleClaim, не ошибка выполнения.
// reasonKey пишется в failure_reason_key только при переходе в failed, не при возврате в очередь.
func (s *Store) Fail(ctx context.Context, id int64, attempt int, cause, reasonKey string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END,
		    finished_at = CASE WHEN attempts >= $2 THEN now() ELSE NULL END,
		    claimed_at = NULL, last_error = $3,
		    failure_reason_key = CASE WHEN attempts >= $2 THEN $5 ELSE NULL END
		WHERE id = $1 AND status = 'running' AND attempts = $4`, id, maxAttempts, cause, attempt, reasonKey)
	if err != nil {
		return fmt.Errorf("export: отметка неудачи заявки %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Срок хранения — от завершения, не от постановки: очередь и мгновенная заявка хранятся одинаково.
func (s *Store) Done(ctx context.Context, id int64, attempt int, rows, bytes int64, truncated bool, ttl time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = 'done', rows_written = $2, bytes = $3, truncated = $4, last_error = '',
		    finished_at = now(), expires_at = now() + $5::interval
		WHERE id = $1 AND status = 'running' AND attempts = $6`,
		id, rows, bytes, truncated, ttl.String(), attempt)
	if err != nil {
		return fmt.Errorf("export: завершение заявки %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Закрывает без права на повтор — для причин, которые повтор не устранит (диск, слишком много групп).
func (s *Store) FailPermanent(ctx context.Context, id int64, attempt int, cause, reasonKey string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = 'failed', finished_at = now(), last_error = $3, failure_reason_key = $4
		WHERE id = $1 AND status = 'running' AND attempts = $2`, id, attempt, cause, reasonKey)
	if err != nil {
		return fmt.Errorf("export: постоянный отказ заявки %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Не тратит попытку: прерванная штатной остановкой сборка — не вина заявки, в отличие от Fail.
func (s *Store) Release(ctx context.Context, id int64, attempt int) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs SET status = 'queued', claimed_at = NULL
		WHERE id = $1 AND status = 'running' AND attempts = $2`, id, attempt)
	if err != nil {
		return fmt.Errorf("export: возврат заявки %d в очередь: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Тот же ErrNotDeletable и для queued/running (файл ещё пишется), и для несуществующего id.
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

// Только выборка: джанитор сам решает порядок — сначала файл, потом MarkExpired.
func (s *Store) DueForExpiry(ctx context.Context) ([]Job, error) {
	rows, err := s.pool.Query(ctx, "SELECT "+jobColumns+`
		FROM export_jobs WHERE status = 'done' AND expires_at < now()
		ORDER BY expires_at LIMIT $1`, s.batch())
	if err != nil {
		return nil, fmt.Errorf("export: заявки на истечение срока: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("export: разбор заявки на истечение срока: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export: заявки на истечение срока: %w", err)
	}
	return out, nil
}

// status='done' делает идемпотентным — уже переведённая заявка повторно не тронется.
func (s *Store) MarkExpired(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE export_jobs SET status = 'expired' WHERE id = ANY($1) AND status = 'done'`, ids); err != nil {
		return fmt.Errorf("export: пометка истёкших заявок: %w", err)
	}
	return nil
}

// Возраст — от finished_at, не created_at: заявка, час простоявшая в очереди,
// чистится не раньше исполненной мгновенно.
func (s *Store) PurgeRows(ctx context.Context, olderThan time.Duration) (int, error) {
	cutoff := time.Now().Add(-olderThan)
	var total int
	for {
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM export_jobs WHERE id IN (
				SELECT id FROM export_jobs
				WHERE status IN ('done','failed','expired') AND finished_at < $1
				LIMIT $2
			)`, cutoff, s.batch())
		if err != nil {
			return total, fmt.Errorf("export: чистка старых заявок: %w", err)
		}
		n := int(tag.RowsAffected())
		total += n
		if n < s.batch() {
			return total, nil
		}
		// Между пачками уступаем: проход не должен монополизировать базу.
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
}

// SQL дублирует auth.Service.UserEmail нарочно — Store не должен тянуть весь auth ради одного email.
func (s *Store) AuthorEmail(ctx context.Context, id int64) (string, error) {
	var email string
	err := s.pool.QueryRow(ctx, "SELECT email FROM users WHERE id = $1", id).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("export: пользователь %d не найден", id)
	}
	if err != nil {
		return "", fmt.Errorf("export: адрес автора %d: %w", id, err)
	}
	return email, nil
}

// Использует джанитор, сверяя файлы каталога с базой, чтобы найти сирот (файл есть, строки нет).
func (s *Store) ExistingIDs(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM export_jobs WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("export: проверка существующих заявок: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("export: разбор существующих заявок: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("export: проверка существующих заявок: %w", err)
	}
	return out, nil
}
