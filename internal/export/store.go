package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// leaseTTL строго больше таймаута сборки файла (jobTimeout воркера):
	// иначе второй инстанс переклеймит заявку, которую первый ещё пишет,
	// и оба одновременно запишут результат.
	leaseTTL = 20 * time.Minute
	// maxAttempts — потолок попыток одной заявки. Заявка, упавшая столько
	// раз подряд, скорее всего бьётся о системную проблему (ClickHouse
	// недоступен, диск полон), а не о случайный сбой — дальше её добивает
	// SweepStale, а не бесконечный переклейм.
	maxAttempts = 3
)

var (
	// ErrNotFound — заявки с таким id нет в таблице.
	ErrNotFound = errors.New("export: заявка не найдена")
	// ErrNotDeletable — заявка ещё не досчитана (queued/running) либо уже
	// удалена: удалять можно только терминальные заявки.
	ErrNotDeletable = errors.New("export: заявка ещё выполняется")
	// ErrStaleClaim — вызывающий пишет результат уже не своей попытки:
	// заявку успели переклеймить (Claim увеличил attempts) или её уже
	// финализировал другой вызов Done/Fail. Воркер, поймавший эту ошибку,
	// обязан молча остановиться — дописывать поверх чужой попытки нельзя.
	ErrStaleClaim = errors.New("export: заявка перехвачена другой попыткой")
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
//
// limit — забота вызывающей стороны, стор его не подменяет: 0 даёт пустой
// список (валидный SQL LIMIT 0), отрицательное значение — ошибку PostgreSQL.
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

// Claim берёт в работу самую старую доступную заявку: свежую из очереди либо
// «running» с протухшей лизой, у которой ещё остались попытки. SELECT ... FOR
// UPDATE SKIP LOCKED внутри одного UPDATE — атомарная операция: две
// параллельные попытки клейма физически не могут захватить одну и ту же
// строку, второй достанется следующая свободная либо ok=false.
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

// SweepStale закрывает заявки, пережившие все попытки: без этого заявка,
// упавшая вместе с инстансом на последней попытке, вечно показывала бы
// «выполняется» — Claim её больше не тронет (attempts >= maxAttempts), а
// статус сам себя не поменяет.
func (s *Store) SweepStale(ctx context.Context) (int, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = 'failed', finished_at = now(),
		    last_error = 'сборка выгрузки не завершилась за отведённое число попыток'
		WHERE status = 'running' AND claimed_at < now() - $1::interval AND attempts >= $2`,
		leaseTTL.String(), maxAttempts)
	if err != nil {
		return 0, fmt.Errorf("export: снятие зависших заявок: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Fail возвращает заявку в очередь, пока попытки не исчерпаны: ждать
// протухания лизы незачем — причина неудачи уже известна здесь и сейчас.
//
// attempt — номер попытки, с которым вызывающий получил заявку от Claim.
// status='running' один не различает ВЛАДЕЛЬЦА текущего running-статуса:
// если лиза протухла и заявку успел переклеймить кто-то другой, статус
// остаётся 'running', но это уже чужая попытка. attempts растёт только в
// Claim, поэтому связка status='running' AND attempts=$attempt однозначно
// подтверждает, что вызывающий всё ещё держит именно ту попытку, что забрал.
// Ноль затронутых строк — не ошибка выполнения, а сигнал ErrStaleClaim:
// заявку либо переклеймили, либо её уже финализировал кто-то другой.
func (s *Store) Fail(ctx context.Context, id int64, attempt int, cause string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = CASE WHEN attempts >= $2 THEN 'failed' ELSE 'queued' END,
		    finished_at = CASE WHEN attempts >= $2 THEN now() ELSE NULL END,
		    claimed_at = NULL, last_error = $3
		WHERE id = $1 AND status = 'running' AND attempts = $4`, id, maxAttempts, cause, attempt)
	if err != nil {
		return fmt.Errorf("export: отметка неудачи заявки %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Done завершает заявку успехом и назначает срок хранения файла от момента
// завершения, а не от постановки в очередь: заявка, простоявшая в очереди
// час, обязана хранить файл столько же, сколько исполненная мгновенно.
//
// attempt фенсит владение попыткой той же связкой status='running' AND
// attempts=$attempt, что и Fail (см. её комментарий) — без этого запоздавший
// Done от воркера, потерявшего лизу, дописал бы свои устаревшие rows/bytes
// поверх заявки, которую уже ведёт следующая попытка.
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

// FailPermanent закрывает заявку без права на повтор: причина не устранится
// следующей попыткой (бюджет диска исчерпан, фильтр резолвится в слишком
// много групп) — три бессмысленных повтора только оттянут момент, когда
// человек увидит внятную причину.
//
// attempt фенсит владение попыткой той же связкой status='running' AND
// attempts=$attempt, что и Fail/Done (см. их комментарии): без этого
// запоздавший постоянный отказ от зомби-вызова (лиза протухла, заявку
// переклеймили) закрыл бы заявку поверх активной попытки, которая её уже
// подхватила и продолжает работать, — дыра шире обычного зомби-Done, потому
// что FailPermanent не ждёт даже исчерпания попыток. Ноль затронутых строк —
// тот же сигнал ErrStaleClaim, что и у Fail/Done, а не ошибка выполнения.
func (s *Store) FailPermanent(ctx context.Context, id int64, attempt int, cause string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE export_jobs
		SET status = 'failed', finished_at = now(), last_error = $3
		WHERE id = $1 AND status = 'running' AND attempts = $2`, id, attempt, cause)
	if err != nil {
		return fmt.Errorf("export: постоянный отказ заявки %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrStaleClaim
	}
	return nil
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
