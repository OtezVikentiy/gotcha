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

// maxPurgeErrorLen — потолок длины текста ошибки, оседающего в заявке. Ошибка
// драйвера ClickHouse может нести весь текст запроса; заявка — не журнал.
const maxPurgeErrorLen = 500

// PurgeQueue — очередь заявок на очистку телеметрии удалённых проектов.
//
// Заявка пишется той же транзакцией PostgreSQL, что удаляет проект (см.
// org.Service.DeleteProject): либо проект удалён и заявка есть, либо не
// удалено ничего. Без этой атомарности удаление, оборванное между двумя
// операторами, оставляло бы телеметрию неадресуемой навсегда — идентификатора
// проекта после каскада взять уже негде.
type PurgeQueue struct {
	pool *pgxpool.Pool
}

// NewPurgeQueue создаёт очередь поверх пула PostgreSQL.
func NewPurgeQueue(pool *pgxpool.Pool) *PurgeQueue { return &PurgeQueue{pool: pool} }

// Enqueue ставит заявки на проекты вне транзакции удаления. Используется
// сверкой сирот (проектов, которых в PostgreSQL уже нет) и тестами; штатный
// путь удаления ставит заявку сам, внутри своей транзакции.
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

// Claim забирает следующую заявку, отмечая попытку. Возвращает ok=false, если
// очередь пуста.
//
// Попытка засчитывается при захвате, а не при успехе: процесс, упавший
// посреди мутаций, обязан оставить след, иначе счётчик попыток показывал бы
// ноль у заявки, над которой бьются сутки.
//
// SKIP LOCKED стоит на случай, если исполнителей всё-таки окажется несколько
// (advisory-лок исполнителя — механизм эксплуатации, а не инвариант схемы):
// две реплики не должны драться за одну строку. Повторное удаление в
// ClickHouse идемпотентно, поэтому даже двойной захват безопасен.
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

// Done снимает заявку. Вызывается ТОЛЬКО после подтверждённого завершения всех
// мутаций: снятая раньше заявка — то же самое, что необработанная, только
// молча.
func (q *PurgeQueue) Done(ctx context.Context, projectID int64) error {
	if _, err := q.pool.Exec(ctx,
		"DELETE FROM project_purge_queue WHERE project_id = $1", projectID); err != nil {
		return fmt.Errorf("telemetry: complete purge request %d: %w", projectID, err)
	}
	return nil
}

// Fail записывает причину отказа в заявку, оставляя её в очереди. Отказаться
// от заявки нельзя ни при каком числе попыток: невыполненное удаление
// персональных данных не та вещь, которую можно списать в потери.
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

// Stats возвращает глубину очереди и возраст самой старой заявки в секундах.
//
// Обе величины уходят в самотелеметрию: очередь, которая не разгребается, —
// это невыполненное обещание удалить данные, и увидеть его можно только так.
// Возраст отдельно от глубины намеренно: очередь из одной заявки, висящей
// третьи сутки, по глубине неотличима от заявки, поставленной минуту назад.
// Пустая очередь даёт (0, 0).
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

// defaultPurgeWorkerInterval — как часто исполнитель заглядывает в очередь.
// Минута: заявка появляется по действию человека, и минутная задержка на
// удалении, которое само идёт минуты, ничего не меняет.
const defaultPurgeWorkerInterval = time.Minute

// purgeWorkerLockID — advisory-лок исполнителя, отдельный от чистильщика
// сущностей (entityJanitorLockID). Он не для корректности — повторное
// ALTER … DELETE идемпотентно, — а чтобы две реплики не гоняли одни и те же
// восемь тяжёлых мутаций одновременно.
const purgeWorkerLockID = 0x676F7471 // "gotq"

// PurgeWorker разгребает очередь заявок на очистку телеметрии удалённых
// проектов.
//
// Живёт рядом с чистильщиком сущностей и устроен так же: собственный
// advisory-лок, проход по тикеру, отказ одной заявки не отменяет остальную
// работу. Отличие одно и существенное: заявку нельзя потерять. Чистильщик
// сущностей на следующем тике встретит те же строки и удалит их снова, а
// потерянная заявка на очистку — это невыполненное требование об удалении
// персональных данных, о котором никто больше не узнает.
type PurgeWorker struct {
	Queue  *PurgeQueue
	Purger *Purger

	// Conn — соединение с ClickHouse для сверки сирот. Без него сверка не
	// выполняется, а очередь разгребается как обычно.
	Conn driver.Conn

	// Interval — период прохода; 0 означает defaultPurgeWorkerInterval.
	Interval time.Duration

	// ReconcileInterval — период сверки сирот; 0 её выключает. Установка, где
	// в ClickHouse пишет что-то помимо продукта, обязана иметь возможность
	// выключить сверку, не выключая очередь.
	ReconcileInterval time.Duration

	purged        atomic.Int64
	depth         atomic.Int64
	oldestSeconds atomic.Int64
}

// Purged — сколько проектов очищено за время жизни процесса.
func (w *PurgeWorker) Purged() int64 { return w.purged.Load() }

// Depth — глубина очереди на момент последнего прохода.
func (w *PurgeWorker) Depth() int64 { return w.depth.Load() }

// OldestSeconds — возраст самой старой заявки на момент последнего прохода.
func (w *PurgeWorker) OldestSeconds() int64 { return w.oldestSeconds.Load() }

// Run разгребает очередь каждые Interval, пока не отменят ctx.
func (w *PurgeWorker) Run(ctx context.Context) {
	interval := w.Interval
	if interval <= 0 {
		interval = defaultPurgeWorkerInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Сверка идёт по своему, редкому тикеру: она нужна на случай, когда заявка
	// не появилась вообще (падение до коммита, ручная правка строк), а это
	// редкое событие. Нулевой период выключает её совсем.
	var reconcileC <-chan time.Time
	if w.ReconcileInterval > 0 {
		rt := time.NewTicker(w.ReconcileInterval)
		defer rt.Stop()
		reconcileC = rt.C
	}

	// Первый проход — сразу: после рестарта заявка, поставленная перед
	// падением, не должна ждать полного периода.
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

// Tick обрабатывает заявки, пока очередь не опустеет, и возвращает число
// обработанных.
//
// Показания наблюдаемости обновляются ДО захвата лока и независимо от него:
// реплика, уступившая проход, обязана отдавать в самотелеметрию тот же ответ
// на вопрос «сколько данных ещё не удалено», что и работающая.
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

	// Лок берётся на соединении и держится до его освобождения: advisory-лок
	// сессионный, и взять его через пул без явного соединения нельзя.
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
			// Заявка остаётся: причина записывается в неё и в лог, следующая
			// попытка — на следующем тике. Выходим из прохода, а не берём
			// следующую заявку: отказ почти всегда общий (ClickHouse
			// недоступен), и перебор очереди только умножил бы ошибки на
			// недоступном хранилище.
			//
			// Причина пишется отдельным контекстом: ctx мог быть уже отменён
			// (остановка процесса), а заявка без записанной причины оставила бы
			// оператора без единственного следа того, почему данные ещё живы.
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

// Reconcile ставит в очередь проекты, чьи данные есть в ClickHouse, а строки
// в PostgreSQL уже нет.
//
// СВЕРКА ТОЛЬКО СТАВИТ ЗАЯВКИ И НИКОГДА НЕ УДАЛЯЕТ САМА. Удаление идёт
// единственным путём — исполнителем очереди, — поэтому ошибка сверки даёт
// лишнюю заявку, а не потерянные данные.
//
// ПОРЯДОК ШАГОВ ОБРАТНОМУ НЕ РАВЕН. Сперва читаются идентификаторы из
// ClickHouse, ПОТОМ — список живых проектов из PostgreSQL. При обратном
// порядке проект, созданный между двумя шагами, отсутствовал бы в списке
// живых, попал бы в разность и был бы поставлен на удаление живым.
//
// ЦЕНА ПЕРЕБОРА ЗАМЕРЕНА, А НЕ ВЫВЕДЕНА ИЗ УСТРОЙСТВА КЛЮЧА. Замер 2026-08-03,
// ClickHouse 25.3, таблица той же схемы и с тем же ключом сортировки, что
// events (5 000 000 строк, 200 различных проектов): read_rows = 5 000 000,
// прочитано 38.15 МиБ, память 362 КиБ, query_duration_ms = 5.
//
// То есть перебор читает КОЛОНКУ project_id целиком, а не засечки индекса —
// ожидание «опрётся на первичный индекс» не подтвердилось. Дёшево это по
// другой причине: колонка одна, она ведущая в ключе сортировки и потому
// сжимается почти нацело (38 МиБ на пять миллионов строк). Цена растёт
// линейно от числа строк; на сотне миллионов это порядка сотни миллисекунд на
// таблицу, что для суточного прохода приемлемо. Если на боевом объёме
// окажется иначе — честный выход сверять по одной таблице за проход, храня в
// исполнителе индекс следующей.
func (w *PurgeWorker) Reconcile(ctx context.Context) (int, error) {
	if w.Conn == nil || w.Queue == nil {
		return 0, nil
	}
	seen := map[int64]struct{}{}
	for _, table := range projectTables {
		// Имя таблицы — из projectTables, литералов этого пакета, а не из
		// пользовательского ввода (параметризовать его нельзя).
		rows, err := w.Conn.Query(ctx, "SELECT DISTINCT project_id FROM "+table)
		if err != nil {
			return 0, fmt.Errorf("telemetry: reconcile: scan %s: %w", table, err)
		}
		for rows.Next() {
			// project_id в ClickHouse — UInt64, а в PostgreSQL bigserial:
			// читаем в беззнаковое и отбрасываем то, что не помещается в
			// int64. Такого значения приём породить не может (идентификатор
			// приходит из PostgreSQL), но мусор, записанный чем-то посторонним,
			// не должен превратиться в отрицательный идентификатор и попасть в
			// заявку на удаление.
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
