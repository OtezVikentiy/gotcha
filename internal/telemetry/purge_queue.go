package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const maxPurgeErrorLen = 500

// Заявка пишется той же транзакцией PostgreSQL, что удаляет проект (см. org.Service.DeleteProject):
// без этой атомарности оборванное удаление оставляло бы телеметрию неадресуемой навсегда.
type PurgeQueue struct {
	pool *pgxpool.Pool
}

func NewPurgeQueue(pool *pgxpool.Pool) *PurgeQueue { return &PurgeQueue{pool: pool} }

func (q *PurgeQueue) Enqueue(ctx context.Context, projectIDs ...int64) error {
	if len(projectIDs) == 0 {
		return nil
	}
	if _, err := q.pool.Exec(ctx, `
		INSERT INTO project_purge_queue (project_id)
		SELECT unnest($1::bigint[])
		ON CONFLICT (project_id) DO NOTHING`, projectIDs); err != nil {
		return fmt.Errorf("telemetry: enqueue purge: %w", err)
	}
	return nil
}

// Попытка засчитывается при захвате, не при успехе — иначе упавший процесс не оставит следа.
// SKIP LOCKED на случай нескольких исполнителей: двойной захват безопасен, DELETE идемпотентен.
func (q *PurgeQueue) Claim(ctx context.Context) (int64, bool, error) {
	var projectID int64
	err := q.pool.QueryRow(ctx, `
		UPDATE project_purge_queue
		SET attempts = attempts + 1, last_attempt_at = now()
		WHERE project_id = (
			SELECT project_id FROM project_purge_queue
			ORDER BY enqueued_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING project_id`).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("telemetry: claim purge request: %w", err)
	}
	return projectID, true, nil
}

// Вызывается ТОЛЬКО после подтверждённого завершения всех мутаций: снятая раньше —
// то же самое, что необработанная заявка, только молча.
func (q *PurgeQueue) Done(ctx context.Context, projectID int64) error {
	if _, err := q.pool.Exec(ctx,
		"DELETE FROM project_purge_queue WHERE project_id = $1", projectID); err != nil {
		return fmt.Errorf("telemetry: complete purge request %d: %w", projectID, err)
	}
	return nil
}

// Отказаться от заявки нельзя ни при каком числе попыток: удаление ПДн не списывается в потери.
func (q *PurgeQueue) Fail(ctx context.Context, projectID int64, cause error) error {
	msg := cause.Error()
	// Режем по границе рун: ошибки в этом дереве пишутся по-русски, и обрезка
	// по байту оставила бы в колонке половину символа.
	if r := []rune(msg); len(r) > maxPurgeErrorLen {
		msg = string(r[:maxPurgeErrorLen])
	}
	if _, err := q.pool.Exec(ctx,
		"UPDATE project_purge_queue SET last_error = $2 WHERE project_id = $1",
		projectID, msg); err != nil {
		return fmt.Errorf("telemetry: record purge failure %d: %w", projectID, err)
	}
	return nil
}

// Возраст отдельно от глубины: заявка, висящая третьи сутки, по одной глубине неотличима
// от только что поставленной. Пустая очередь даёт (0, 0).
func (q *PurgeQueue) Stats(ctx context.Context) (int64, int64, error) {
	var depth, oldest int64
	if err := q.pool.QueryRow(ctx, `
		SELECT count(*),
		       coalesce(extract(epoch FROM now() - min(enqueued_at))::bigint, 0)
		FROM project_purge_queue`).Scan(&depth, &oldest); err != nil {
		return 0, 0, fmt.Errorf("telemetry: purge queue stats: %w", err)
	}
	return depth, oldest, nil
}

const defaultPurgeWorkerInterval = time.Minute

// Отдельный от entityJanitorLockID: не для корректности (DELETE идемпотентен), а чтобы
// реплики не гоняли одни и те же тяжёлые мутации одновременно.
const purgeWorkerLockID = 0x676F7471 // "gotq"

// В отличие от чистильщика сущностей, заявку нельзя потерять: потерянная — невыполненное
// удаление ПДн, о котором никто больше не узнает.
type PurgeWorker struct {
	Queue  *PurgeQueue
	Purger *Purger

	Conn driver.Conn

	Interval time.Duration

	ReconcileInterval time.Duration

	purged        atomic.Int64
	depth         atomic.Int64
	oldestSeconds atomic.Int64
}

func (w *PurgeWorker) Purged() int64 { return w.purged.Load() }

func (w *PurgeWorker) Depth() int64 { return w.depth.Load() }

func (w *PurgeWorker) OldestSeconds() int64 { return w.oldestSeconds.Load() }

func (w *PurgeWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultPurgeWorkerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Нулевой ReconcileInterval выключает сверку: reconcileC остаётся nil, и приём из
	// него в select ниже никогда не сработает.
	var reconcileC <-chan time.Time
	if w.ReconcileInterval > 0 {
		rt := time.NewTicker(w.ReconcileInterval)
		defer rt.Stop()
		reconcileC = rt.C
	}

	w.tickLogged(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.tickLogged(ctx)
		case <-reconcileC:
			if _, err := w.Reconcile(ctx); err != nil {
				slog.Error("telemetry: purge worker: reconcile failed", "error", err)
			}
		}
	}
}

func (w *PurgeWorker) tickLogged(ctx context.Context) {
	n, err := w.Tick(ctx)
	if err != nil {
		slog.Error("telemetry: purge worker: request failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("telemetry: purge worker: projects purged", "projects", n)
	}
}

// Показания наблюдаемости обновляются ДО захвата лока: реплика, уступившая проход, обязана
// отдавать тот же ответ на «сколько данных ещё не удалено», что и работающая.
func (w *PurgeWorker) Tick(ctx context.Context) (int, error) {
	if w.Queue == nil || w.Purger == nil {
		return 0, nil
	}
	if depth, oldest, err := w.Queue.Stats(ctx); err == nil {
		w.depth.Store(depth)
		w.oldestSeconds.Store(oldest)
	} else {
		slog.Warn("telemetry: purge worker: stats failed", "error", err)
	}

	conn, err := w.Queue.pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("telemetry: purge worker: acquire: %w", err)
	}
	defer conn.Release()

	// Advisory-лок сессионный: берётся на одном соединении и держится до его освобождения.
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(purgeWorkerLockID)).Scan(&locked); err != nil {
		return 0, fmt.Errorf("telemetry: purge worker: lock: %w", err)
	}
	if !locked {
		// Проход идёт на другой реплике. Это нормальная работа, не сбой.
		return 0, nil
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(purgeWorkerLockID)); err != nil {
			slog.Warn("telemetry: purge worker: unlock failed", "error", err)
		}
	}()

	var done int
	for {
		projectID, ok, err := w.Queue.Claim(ctx)
		if err != nil {
			return done, err
		}
		if !ok {
			return done, nil
		}
		if err := w.Purger.PurgeProject(ctx, projectID); err != nil {
			// Прерываем проход, не берём следующую заявку: отказ почти всегда общий.
			// Отдельный контекст: ctx мог быть уже отменён, а причина обязана записаться.
			failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			ferr := w.Queue.Fail(failCtx, projectID, err)
			cancel()
			if ferr != nil {
				slog.Error("telemetry: purge worker: failure not recorded",
					"project_id", projectID, "error", ferr)
			}
			return done, fmt.Errorf("telemetry: purge worker: project %d: %w", projectID, err)
		}
		if err := w.Queue.Done(ctx, projectID); err != nil {
			return done, err
		}
		w.purged.Add(1)
		done++
		slog.Info("telemetry: purge worker: project telemetry removed", "project_id", projectID)
	}
}

// Только ставит заявки, никогда не удаляет сама. Порядок шагов обязателен: id из ClickHouse
// читаются ДО живых проектов PostgreSQL — иначе созданный между шагами проект попал бы в сироты.
func (w *PurgeWorker) Reconcile(ctx context.Context) (int, error) {
	if w.Conn == nil || w.Queue == nil {
		return 0, nil
	}
	seen := map[int64]struct{}{}
	for _, table := range projectTables {
		rows, err := w.Conn.Query(ctx, "SELECT DISTINCT project_id FROM "+table)
		if err != nil {
			return 0, fmt.Errorf("telemetry: reconcile: scan %s: %w", table, err)
		}
		for rows.Next() {
			// project_id — UInt64 в ClickHouse, bigserial в PostgreSQL: значения вне int64
			// отбрасываются, а не превращаются в отрицательный идентификатор.
			var id uint64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("telemetry: reconcile: scan %s: %w", table, err)
			}
			if id > math.MaxInt64 {
				slog.Warn("telemetry: reconcile: project id out of range, skipped",
					"table", table, "project_id", id)
				continue
			}
			seen[int64(id)] = struct{}{}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return 0, fmt.Errorf("telemetry: reconcile: scan %s: %w", table, err)
		}
	}
	if len(seen) == 0 {
		return 0, nil
	}

	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	rows, err := w.Queue.pool.Query(ctx, "SELECT id FROM projects WHERE id = ANY($1)", ids)
	if err != nil {
		return 0, fmt.Errorf("telemetry: reconcile: live projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("telemetry: reconcile: live projects: %w", err)
		}
		delete(seen, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("telemetry: reconcile: live projects: %w", err)
	}

	orphans := make([]int64, 0, len(seen))
	for id := range seen {
		orphans = append(orphans, id)
	}
	if len(orphans) == 0 {
		return 0, nil
	}
	if err := w.Queue.Enqueue(ctx, orphans...); err != nil {
		return 0, err
	}
	slog.Warn("telemetry: reconcile: telemetry of deleted projects found",
		"projects", len(orphans))
	return len(orphans), nil
}
