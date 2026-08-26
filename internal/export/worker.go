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
	// defaultTickInterval — период опроса очереди, если Config.TickInterval
	// не задан. Раз в 5 секунд, как у остальных фоновых воркеров проекта
	// (notify/worker, uptime/scheduler).
	defaultTickInterval = 5 * time.Second
	// defaultJobTimeout — потолок сборки одного файла, если Config.JobTimeout
	// не задан. Строго меньше leaseTTL (store.go, 20 минут): иначе второй
	// инстанс переклеймит заявку, которую первый ещё пишет, и оба
	// одновременно запишут результат в один и тот же путь. Инвариант для
	// этого значения по умолчанию проверяется в init(); для значения,
	// заданного через Config, — в Config.Validate(), которую зовёт каждый
	// Tick: конфигурация приходит из окружения (§10 спеки) и может развести
	// числа так же легко, как правка констант.
	defaultJobTimeout = 15 * time.Minute
	// advisoryLockKey — произвольный, но постоянный ключ сессионного лока:
	// "expo" в ASCII. Один воркер под локом — вторая реплика молча уступает
	// проход, а не пишет тот же файл параллельно.
	advisoryLockKey = 0x6578706F
)

func init() {
	if defaultJobTimeout >= leaseTTL {
		panic("export: defaultJobTimeout обязан быть строго меньше leaseTTL")
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

// reasonDiskFull/reasonTooManyGroups/reasonInternal — ключи i18n.T() причины
// отказа для письма автору (Job.FailureReasonKey, см. её докблок и
// mailPayload в notify.go).
//
// Технические cause-строки, которые process()/writeFile() заворачивают в
// fail()/failPermanent() (например "подсчёт занятого места в каталоге
// выгрузок: %w" или "создание временного файла выгрузки: %w"), остаются
// техническим текстом last_error для БД и лога — это внутренняя диагностика
// для оператора, разбирающего инцидент по базе, не для автора заявки.
// Письмо же обязано быть переведено (спека фичи, §9: «Письмо о неудаче —
// тоже, с причиной», «Все тексты — ключи exports.* … тест паритета
// зелёный») — раньше notifyFailed передавала в письмо ЭТУ ЖЕ техническую
// русскую строку напрямую (см. её докблок в задаче 14 report — было
// найдено гейтом TestNoCyrillicUserFacingLiterals: письмо на английской
// локали получало необъяснённый русский обрывок вперемешку с переводом).
// Три ключа — все различимые для автора причины: нехватка места (можно
// подождать), слишком широкий фильтр (можно сузить самому) и всё
// остальное — внутренняя ошибка сборки, requires no action от автора кроме
// повторной попытки/обращения в поддержку.
const (
	reasonDiskFull      = "exports.mail.failed.reason.disk_full"
	reasonTooManyGroups = "exports.mail.failed.reason.too_many_groups"
	reasonInternal      = "exports.mail.failed.reason.internal"
)

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
	// TickInterval — период опроса очереди; 0 — defaultTickInterval.
	// Полю, а не константе, живёт ради тестируемости Run() без ожидания
	// боевых 5 секунд (по образцу internal/notify/worker.go: Worker.Interval).
	TickInterval time.Duration
	// JobTimeout — потолок сборки одного файла; 0 — defaultJobTimeout.
	// Инъекция в тестах позволяет проверить, что деградация ctx (истёк
	// дедлайн тика) действительно останавливает финализацию заявки, а не
	// ждать боевых 15 минут. Validate() отдельно следит, чтобы инъекция не
	// нарушила JobTimeout < leaseTTL.
	JobTimeout time.Duration
}

// tickInterval — период опроса очереди с учётом Config.TickInterval.
func (c Config) tickInterval() time.Duration {
	if c.TickInterval > 0 {
		return c.TickInterval
	}
	return defaultTickInterval
}

// jobTimeout — потолок сборки одного файла с учётом Config.JobTimeout.
func (c Config) jobTimeout() time.Duration {
	if c.JobTimeout > 0 {
		return c.JobTimeout
	}
	return defaultJobTimeout
}

// Validate проверяет конфигурацию на нарушения, которые нельзя пропустить
// тихо. MaxRows на уровне или выше eventStreamSafetyLimit (source_events.go)
// обесценивает Truncated: источник событий физически не отдаст больше
// eventStreamSafetyLimit строк, поток закончится «естественно» раньше, чем
// счётчик заявки дойдёт до своего потолка, и Truncated останется false —
// ровно то самое «молча неполная выгрузка», которое запрещает §8 спеки.
// JobTimeout не строже leaseTTL воскрешает зомби-переклейм, ради которого
// заведён init()-сторож для значения по умолчанию.
func (c Config) Validate() error {
	if c.MaxRows >= eventStreamSafetyLimit {
		return fmt.Errorf("export: конфигурация: MaxRows (%d) обязан быть строго меньше защитного предела потока событий (%d) — иначе усечение по этому пределу проходит без Truncated=true",
			c.MaxRows, eventStreamSafetyLimit)
	}
	if jt := c.jobTimeout(); jt >= leaseTTL {
		return fmt.Errorf("export: конфигурация: JobTimeout (%s) обязан быть строго меньше leaseTTL (%s)", jt, leaseTTL)
	}
	return nil
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
	ticker := time.NewTicker(w.Cfg.tickInterval())
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
	if err := w.Cfg.Validate(); err != nil {
		return fmt.Errorf("export: воркер: %w", err)
	}

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
	jobCtx, cancel := context.WithTimeout(ctx, w.Cfg.jobTimeout())
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
// успешного закрытия писателя.
func (w *Worker) process(ctx context.Context, job Job) {
	partPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.part", job.ID))
	finalPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.%s", job.ID, job.Format.Ext()))

	used, err := dirSize(w.Cfg.Dir)
	if err != nil {
		w.fail(ctx, job, fmt.Errorf("подсчёт занятого места в каталоге выгрузок: %w", err), reasonInternal)
		return
	}
	if used >= w.Cfg.DiskBudget {
		w.failPermanent(ctx, job, "на диске не осталось места под выгрузку: исчерпан общий бюджет каталога", reasonDiskFull)
		return
	}

	res, err := w.writeFile(ctx, job, partPath)
	if err != nil {
		_ = os.Remove(partPath)
		switch {
		case errors.Is(err, ErrTooManyIssues):
			// Единственная постоянная причина, которую автор может
			// устранить сам (сузить фильтр) — отдельный ключ письма, а не
			// общий "внутренняя ошибка" (см. §8 спеки: «Упор в потолок id
			// групп даёт отказ с просьбой сузить фильтр»).
			w.failPermanent(ctx, job, err.Error(), reasonTooManyGroups)
		case errors.Is(err, ErrPermanent) || errors.Is(err, ErrMaxIssueIDsNotConfigured):
			w.failPermanent(ctx, job, err.Error(), reasonInternal)
		default:
			w.fail(ctx, job, err, reasonInternal)
		}
		return
	}

	// Заявку могли удалить, пока писался файл: отдельного перечитывания
	// перед rename нет — Done ниже фенсит владение по id+status='running'+
	// attempts тем же способом, что и Fail (см. их комментарии в store.go),
	// и при удалённой строке получит те же 0 затронутых строк → ErrStaleClaim,
	// что и при переклейме. Разводить эти два случая незачем: оба ведут к
	// одному и тому же — файл убирается, заявка Done не считается.
	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)
		w.fail(ctx, job, fmt.Errorf("переименование файла выгрузки: %w", err), reasonInternal)
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
//
// Notify зовётся, только когда попытки исчерпаны (job.Attempts достиг
// maxAttempts — того же порога, что Store.Fail применяет в SQL): автору
// заявки, упавшей временно, письмо на КАЖДОЙ из трёх попыток было бы спамом
// по поводу состояния, которое воркер ещё может исправить сам следующим
// тиком.
//
// reasonKey — ключ i18n.T() для письма (см. reasonDiskFull и соседние
// константы), отдельно от cause: cause.Error() остаётся техническим
// текстом last_error (лог/БД), reasonKey — переведённая причина для автора.
func (w *Worker) fail(ctx context.Context, job Job, cause error, reasonKey string) {
	if err := w.Store.Fail(ctx, job.ID, job.Attempts, cause.Error()); err != nil {
		if !errors.Is(err, ErrStaleClaim) {
			slog.Warn("export: воркер: запись неудачи", "job_id", job.ID, "err", err)
		}
		return
	}
	if job.Attempts < maxAttempts {
		return
	}
	w.notifyFailed(ctx, job, cause.Error(), reasonKey)
}

// failPermanent закрывает заявку без права на повтор — причина не устранится
// следующей попыткой (см. ErrPermanent). attempt фенсит владение попыткой
// так же, как в fail/Store.Fail: без него запоздавший постоянный отказ от
// зомби-вызова закрыл бы заявку поверх активной попытки, которая её уже
// переклеймила и продолжает работать. ErrStaleClaim по той же причине не
// логируется как сбой воркера — лизу потеряли, и это уже не наша забота.
//
// Notify зовётся сразу — в отличие от fail(), FailPermanent не оставляет
// заявке права на повтор ни на какой попытке, значит первая же и есть
// последняя.
//
// reasonKey — см. докблок fail() выше: тот же смысл, cause здесь уже string,
// а не error.
func (w *Worker) failPermanent(ctx context.Context, job Job, cause string, reasonKey string) {
	if err := w.Store.FailPermanent(ctx, job.ID, job.Attempts, cause); err != nil {
		if !errors.Is(err, ErrStaleClaim) {
			slog.Warn("export: воркер: постоянный отказ", "job_id", job.ID, "err", err)
		}
		return
	}
	w.notifyFailed(ctx, job, cause, reasonKey)
}

// notifyFailed собирает снимок терминально упавшей заявки для w.Notify —
// общая часть fail()/failPermanent(): оба доводят Job до статуса failed с
// причиной, различается только источник cause (error против string).
// FailureReasonKey — единственное поле Job, которое пишет notifyFailed, а не
// process()/store.go: не персистится, нужно только этому снимку (см. её
// докблок в job.go).
func (w *Worker) notifyFailed(ctx context.Context, job Job, cause, reasonKey string) {
	if w.Notify == nil {
		return
	}
	failed := job
	failed.Status = StatusFailed
	failed.FailureReasonKey = reasonKey
	failed.LastError = cause
	w.Notify(ctx, failed)
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
		// job.IncludePII — снимок галки ЭТОЙ заявки, а не свойство w.Events
		// (тот один и тот же на весь процесс, см. NewEventSource): заявки с
		// разным значением галки идут через один источник одна за другой.
		return w.Events.Stream(ctx, job.ProjectID, job.ScopeIssueID, job.IncludePII, job.Params, fn)
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
