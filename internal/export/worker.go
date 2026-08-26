package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// tickInterval — период опроса очереди. Раз в 5 секунд, как у остальных
	// фоновых воркеров проекта (notify/worker, uptime/scheduler).
	tickInterval = 5 * time.Second
	// jobTimeout — потолок сборки одного файла. Строго меньше leaseTTL
	// (store.go, 20 минут): иначе второй инстанс переклеймит заявку, которую
	// первый ещё пишет, и оба одновременно запишут результат в один и тот же
	// путь. Инвариант проверяется в init(): развести константы правкой в
	// будущем не должно быть незаметно.
	jobTimeout = 15 * time.Minute
	// advisoryLockKey — произвольный, но постоянный ключ сессионного лока:
	// "expo" в ASCII. Один воркер под локом — вторая реплика молча уступает
	// проход, а не пишет тот же файл параллельно.
	advisoryLockKey = 0x6578706F
)

func init() {
	if jobTimeout >= leaseTTL {
		panic("export: jobTimeout обязан быть строго меньше leaseTTL")
	}
}

// ErrPermanent — обёртка для причин, которые не имеет смысла повторять:
// источники и писатели заворачивают в него ошибки конфигурации и данных,
// которые не устранит повторная попытка (в отличие от «ClickHouse
// недоступен»). Три бессмысленных повтора только оттянут момент, когда
// человек увидит внятную причину отказа.
var ErrPermanent = errors.New("export: постоянный отказ сборки выгрузки")

// errLimitReached — внутренний сентинел остановки обхода источника при
// достижении потолка строк или байт заявки. Наружу не выходит: process
// разбирает его сам и не отдаёт вызывающему как настоящую ошибку.
var errLimitReached = errors.New("export: достигнут потолок заявки")

// Config — параметры воркера, приходящие из окружения (§10 спеки).
type Config struct {
	// Dir — каталог, куда пишутся файлы выгрузки и временные .part.
	Dir string
	// TTL — срок хранения готового файла от момента завершения заявки.
	TTL time.Duration
	// MaxRows — потолок строк одной выгрузки: упор в него ставит
	// Truncated = true и останавливает обход детерминированно.
	MaxRows int64
	// MaxBytes — потолок размера файла, тот же смысл, что у MaxRows.
	MaxBytes int64
	// DiskBudget — суммарный бюджет каталога Dir: проверяется до начала
	// записи, переполнение — постоянный отказ, а не частично записанный файл.
	DiskBudget int64
}

// Worker — единственный (per-инстанс, под advisory lock) исполнитель очереди
// export_jobs: раз в tickInterval берёт одну заявку и собирает по ней файл.
type Worker struct {
	Store  *Store
	Pool   *pgxpool.Pool
	Issues IssueSource
	Events EventSource
	Cfg    Config
	// Notify вызывается после успешного завершения заявки — письмо автору
	// (внутренности see internal/notify, задача 12). nil допустим: в тестах
	// воркера почта не нужна.
	Notify func(context.Context, Job)
}

// Run крутит тикер до отмены ctx. Ошибка одного тика не останавливает
// воркер — она уже осела в заявке (Fail/FailPermanent) либо в логе, следующий
// тик просто попробует снова.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Tick(ctx); err != nil {
				slog.Warn("export: воркер: тик", "err", err)
			}
		}
	}
}

// Tick обрабатывает не больше одной заявки. Ошибка возвращается только за
// сбои инфраструктуры самого тика (соединение, lock, клейм) — неудача
// сборки конкретной заявки уже записана в неё Fail/FailPermanent и наружу
// как error не всплывает: воркер продолжает крутиться дальше.
func (w *Worker) Tick(ctx context.Context) error {
	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("export: воркер: получение соединения: %w", err)
	}
	defer conn.Release()

	// Лок сессионный и берётся на явном соединении: через пул без него
	// каждый QueryRow мог бы уйти на другое соединение и лок бы не держался.
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(advisoryLockKey)).Scan(&locked); err != nil {
		return fmt.Errorf("export: воркер: advisory lock: %w", err)
	}
	if !locked {
		// Проход идёт на другой реплике — это нормальная работа, не сбой.
		return nil
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(advisoryLockKey)); err != nil {
			slog.Warn("export: воркер: снятие advisory lock", "err", err)
		}
	}()

	// Ctx с дедлайном покрывает все шаги тика вплоть до Done/Fail включительно:
	// context.Background() здесь означал бы зомби-воркер, который дописывает
	// файл уже после того, как процесс должен был остановиться.
	jobCtx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()

	if _, err := w.Store.SweepStale(jobCtx); err != nil {
		slog.Warn("export: воркер: sweep stale", "err", err)
	}

	job, ok, err := w.Store.Claim(jobCtx)
	if err != nil {
		return fmt.Errorf("export: воркер: клейм заявки: %w", err)
	}
	if !ok {
		return nil
	}

	w.process(jobCtx, job)
	return nil
}

// process собирает файл по одной уже заклеймленной заявке и переводит её в
// терминальный статус. Порядок шагов зеркалит §5/§10 спеки: бюджет диска —
// до записи, временный файл — во время, атомарный rename — только после
// успешного закрытия и повторной проверки, что заявку не удалили.
func (w *Worker) process(ctx context.Context, job Job) {
	partPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.part", job.ID))
	finalPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.%s", job.ID, job.Format.Ext()))

	used, err := dirSize(w.Cfg.Dir)
	if err != nil {
		w.fail(ctx, job, fmt.Errorf("подсчёт занятого места в каталоге выгрузок: %w", err))
		return
	}
	if used >= w.Cfg.DiskBudget {
		w.failPermanent(ctx, job, "на диске не осталось места под выгрузку: исчерпан общий бюджет каталога")
		return
	}

	res, err := w.writeFile(ctx, job, partPath)
	if err != nil {
		_ = os.Remove(partPath)
		if errors.Is(err, ErrPermanent) || errors.Is(err, ErrTooManyIssues) || errors.Is(err, ErrMaxIssueIDsNotConfigured) {
			w.failPermanent(ctx, job, err.Error())
		} else {
			w.fail(ctx, job, err)
		}
		return
	}

	// Заявку могли удалить, пока писался файл: переименовывать в этом случае
	// нечего, а оставленный .part на диске — мусор без хозяина.
	if _, err := w.Store.Get(ctx, job.ID); err != nil {
		_ = os.Remove(partPath)
		if !errors.Is(err, ErrNotFound) {
			slog.Warn("export: воркер: перечитывание заявки перед rename", "job_id", job.ID, "err", err)
		}
		return
	}

	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)
		w.fail(ctx, job, fmt.Errorf("переименование файла выгрузки: %w", err))
		return
	}

	if err := w.Store.Done(ctx, job.ID, job.Attempts, res.rows, res.bytes, res.truncated, w.Cfg.TTL); err != nil {
		if errors.Is(err, ErrStaleClaim) {
			// Лизу потеряли уже после того, как файл лёг на диск: заявка
			// теперь чужая (или уже кем-то финализирована), а файл без
			// подтверждённого владельца скачиваемым оставаться не должен.
			_ = os.Remove(finalPath)
			return
		}
		slog.Warn("export: воркер: завершение заявки", "job_id", job.ID, "err", err)
		return
	}

	if w.Notify != nil {
		done := job
		done.Status = StatusDone
		done.RowsWritten, done.Bytes, done.Truncated = res.rows, res.bytes, res.truncated
		w.Notify(ctx, done)
	}
}

// fail помечает заявку временным отказом: попытки ещё есть — вернётся в
// очередь, исчерпаны — станет failed сама Store.Fail. ErrStaleClaim не
// логируется как сбой воркера: лизу потеряли, и это уже не наша забота.
func (w *Worker) fail(ctx context.Context, job Job, cause error) {
	if err := w.Store.Fail(ctx, job.ID, job.Attempts, cause.Error()); err != nil && !errors.Is(err, ErrStaleClaim) {
		slog.Warn("export: воркер: запись неудачи", "job_id", job.ID, "err", err)
	}
}

// failPermanent закрывает заявку без права на повтор — причина не устранится
// следующей попыткой (см. ErrPermanent).
func (w *Worker) failPermanent(ctx context.Context, job Job, cause string) {
	if err := w.Store.FailPermanent(ctx, job.ID, cause); err != nil {
		slog.Warn("export: воркер: постоянный отказ", "job_id", job.ID, "err", err)
	}
}

// writeResult — что вынесено из-под временного файла для Done.
type writeResult struct {
	rows      int64
	bytes     int64
	truncated bool
}

// writeFile создаёт временный .part, стримит в него источник заявки через
// Writer нужного формата и закрывает файл fsync'ом. Файл по любой ошибке
// остаётся закрытым, но не переименованным — удаление .part на совести
// вызывающего (process), чтобы writeFile отвечал только за содержимое файла.
func (w *Worker) writeFile(ctx context.Context, job Job, partPath string) (writeResult, error) {
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return writeResult{}, fmt.Errorf("создание временного файла выгрузки: %w", err)
	}

	cw := &byteCounter{w: f}
	wr, err := NewWriter(cw, job.Format, columnsFor(job.Kind))
	if err != nil {
		f.Close()
		return writeResult{}, fmt.Errorf("создание писателя выгрузки: %w", err)
	}

	var res writeResult
	streamErr := w.stream(ctx, job, func(rec Record) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Потолок строк проверяется ДО записи: он должен обрезать ровно на
		// MaxRows строке, а не на MaxRows+1.
		if w.Cfg.MaxRows > 0 && res.rows >= w.Cfg.MaxRows {
			res.truncated = true
			return errLimitReached
		}
		if err := wr.Write(rec); err != nil {
			return fmt.Errorf("запись строки выгрузки: %w", err)
		}
		res.rows++
		// Потолок байт проверяется ПОСЛЕ записи: заранее размер строки не
		// известен, а строка, из-за которой файл перевалил через потолок,
		// всё равно должна попасть в него — иначе Bytes окажется больше
		// заявленного MaxBytes без единой причины в файле.
		if w.Cfg.MaxBytes > 0 && cw.n >= w.Cfg.MaxBytes {
			res.truncated = true
			return errLimitReached
		}
		return nil
	})
	if streamErr != nil && !errors.Is(streamErr, errLimitReached) {
		f.Close()
		return writeResult{}, fmt.Errorf("чтение источника выгрузки: %w", streamErr)
	}

	if err := wr.Close(); err != nil {
		f.Close()
		return writeResult{}, fmt.Errorf("закрытие писателя выгрузки: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return writeResult{}, fmt.Errorf("fsync временного файла выгрузки: %w", err)
	}
	if err := f.Close(); err != nil {
		return writeResult{}, fmt.Errorf("закрытие временного файла выгрузки: %w", err)
	}
	res.bytes = cw.n
	return res, nil
}

// stream выбирает источник по виду заявки. ScopeIssueID — только у событий:
// у групп область всегда «проект целиком с фильтром», своей группы для
// выгрузки issues не бывает.
func (w *Worker) stream(ctx context.Context, job Job, fn func(Record) error) error {
	switch job.Kind {
	case KindIssues:
		if w.Issues == nil {
			return fmt.Errorf("%w: источник групп не настроен", ErrPermanent)
		}
		return w.Issues.Stream(ctx, job.ProjectID, job.Params, fn)
	case KindEvents:
		if w.Events == nil {
			return fmt.Errorf("%w: источник событий не настроен", ErrPermanent)
		}
		return w.Events.Stream(ctx, job.ProjectID, job.ScopeIssueID, job.Params, fn)
	default:
		return fmt.Errorf("%w: неизвестный вид выгрузки %q", ErrPermanent, job.Kind)
	}
}

// columnsFor — колонки CSV для вида заявки (JSON/NDJSON их игнорируют, см.
// NewWriter).
func columnsFor(k Kind) []string {
	if k == KindEvents {
		return EventColumns()
	}
	return IssueColumns()
}

// dirSize суммирует размеры файлов каталога выгрузок (без рекурсии — в
// каталоге лежат только .part и готовые файлы, подкаталогов не бывает).
func dirSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

// byteCounter считает записанные байты, не влияя на данные, — Bytes
// заявки должен отражать реальный размер файла (BOM/скобки/разделители
// включительно), а не только сумму значений колонок.
type byteCounter struct {
	w io.Writer
	n int64
}

func (c *byteCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
