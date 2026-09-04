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
	// terminalWriteTimeout — бюджет detachTimeout() на саму запись терминала
	// в PG (P2-OPS-5): небольшой (5с) запас с лихвой хватает на здоровую БД,
	// но не даёт зомби-записи зависнуть навсегда, если процесс останавливают,
	// а БД в этот момент недоступна.
	terminalWriteTimeout = 5 * time.Second
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

// KnownFailureReasonKey — сверка ключа из failure_reason_key export_jobs
// (P2-UX-2 аудита) с множеством, которое вправе туда записать
// fail()/failPermanent()/Store.SweepStale ниже. Веб-слой обязан звать её
// перед i18n.T() на значении из БД (см. докблок Job.FailureReasonKey в
// job.go): i18n.T() на неизвестном ключе возвращает сам ключ как есть, а не
// перевод, и без этой проверки повреждённая или устаревшая (миграция назад
// и вперёд, ручная правка) строка стала бы техническим идентификатором,
// показанным пользователю напрямую — той самой утечкой техтекста, что уже
// была отдельной находкой аудита для last_error.
func KnownFailureReasonKey(key string) bool {
	switch key {
	case reasonDiskFull, reasonTooManyGroups, reasonInternal:
		return true
	}
	return false
}

// FailureReasonKeys — то же множество, что проверяет KnownFailureReasonKey,
// экспортированное для guards.TestExportFailureReasonKeysResolve
// (internal/guards/i18n_dynamic_test.go). Оба места, где ключ реально уходит
// в i18n.T (страница «Выгрузки», exports.templ:115, и письмо автору,
// notify.go:111), принимают готовую СТРОКУ из Job.FailureReasonKey, а не
// литерал и не идентификатор — общий сканер каталога (i18n_keys_test.go)
// такой вызов не видит в принципе, и без отдельной проверки ключ без
// перевода доехал бы до пользователя сырым текстом молча (находка волны 2
// полного аудита, кластер 8/10 DEDUP-P1.md).
var FailureReasonKeys = []string{reasonDiskFull, reasonTooManyGroups, reasonInternal}

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
// MaxRows/MaxBytes <= 0 — та же дыра с другой стороны (P2-OPS-1): worker.go
// гасит собственный потолок условием "> 0", то есть 0 ЗДЕСЬ не значит
// "без лимита" (в отличие от задокументированной конвенции проекта у
// GOTCHA_DIST_RATE_PER_MIN/*_RETENTION_DAYS) — поток всё равно
// обрывается на eventStreamSafetyLimit, но молча. DiskBudget <= 0 (P2-OPS-2)
// делает "used >= budget" истинным на пустом каталоге — failPermanent для
// каждой заявки без единой попытки. TTL <= 0 — expires_at не позже now(),
// ближайший тик джанитора сносит файл раньше, чем автор успевает его
// скачать, хотя заявка отчиталась успехом. JobTimeout не строже leaseTTL
// воскрешает зомби-переклейм, ради которого заведён init()-сторож для
// значения по умолчанию.
func (c Config) Validate() error {
	if c.MaxRows <= 0 {
		return fmt.Errorf("export: конфигурация: MaxRows (%d) обязан быть положительным — здесь 0 не значит «без лимита», а тихо включает усечение по защитному пределу потока событий без Truncated=true",
			c.MaxRows)
	}
	if c.MaxRows >= eventStreamSafetyLimit {
		return fmt.Errorf("export: конфигурация: MaxRows (%d) обязан быть строго меньше защитного предела потока событий (%d) — иначе усечение по этому пределу проходит без Truncated=true",
			c.MaxRows, eventStreamSafetyLimit)
	}
	if c.MaxBytes <= 0 {
		return fmt.Errorf("export: конфигурация: MaxBytes (%d) обязан быть положительным — здесь 0 не значит «без лимита», а выключает собственный потолок размера файла",
			c.MaxBytes)
	}
	if c.DiskBudget <= 0 {
		return fmt.Errorf("export: конфигурация: DiskBudget (%d) обязан быть положительным — при 0 или отрицательном значении «занято >= бюджет» истинно на пустом каталоге, и каждая заявка отказывает без единой попытки",
			c.DiskBudget)
	}
	if c.TTL <= 0 {
		return fmt.Errorf("export: конфигурация: TTL (%s) обязан быть положительным — при 0 файл считается истёкшим сразу после сборки, и ближайший тик джанитора сносит его раньше, чем автор успеет скачать",
			c.TTL)
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
	// FreeBytes сообщает объём реального свободного места на файловой
	// системе, содержащей Cfg.Dir (P2-OPS-4 аудита: pgdata/chdata/exportdata
	// в поставляемом docker-compose делят одну ФС хоста, и одного бюджета из
	// env недостаточно). nil — используется боевая platformFreeBytes
	// (diskfree.go/diskfree_linux.go/diskfree_other.go); поле существует,
	// чтобы тесты могли подменить её детерминированным значением, не завися
	// от реального свободного места файловой системы, на которой гоняются
	// тесты.
	FreeBytes func(dir string) (free int64, ok bool, err error)
}

// freeBytes — реализация Worker.FreeBytes с учётом nil-поля (см. его
// докблок).
func (w *Worker) freeBytes(dir string) (int64, bool, error) {
	if w.FreeBytes != nil {
		return w.FreeBytes(dir)
	}
	return freeBytes(dir)
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
		// detachTimeout(ctx), а не ctx напрямую (P2-OPS-5): при штатной
		// остановке процесса ctx (Run-level) уже отменён к этому моменту —
		// без детача снятие лока падало бы с ошибкой отменённого контекста
		// на КАЖДОМ деплое и логировало бы WARN, приучая оператора
		// игнорировать предупреждения о реальных сбоях. Лок сессионный и
		// освобождается вместе с закрытием соединения (conn.Release() чуть
		// ниже) в любом случае — это не более чем аккуратное снятие пораньше,
		// поэтому короткий отвязанный таймаут здесь так же уместен, как и у
		// терминальных записей заявки.
		uctx, cancel := detachTimeout(ctx)
		defer cancel()
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", int64(advisoryLockKey)); err != nil {
			slog.Warn("export: воркер: снятие advisory lock", "err", err)
		}
	}()

	// Ctx с дедлайном покрывает все шаги тика вплоть до самой сборки файла:
	// context.Background() здесь означал бы зомби-воркер, который дописывает
	// файл уже после того, как процесс должен был остановиться. Терминальная
	// ЗАПИСЬ ИТОГА (Done/Fail/FailPermanent/Release) — исключение: она обязана
	// дойти до PG, даже если jobCtx уже умер (собственный таймаут сборки или
	// отмена ctx при остановке процесса), и берёт detachTimeout() вместо
	// jobCtx напрямую (P2-OPS-5, см. process()/fail()/failPermanent()/release()).
	jobCtx, cancel := context.WithTimeout(ctx, w.Cfg.jobTimeout())
	defer cancel()

	swept, err := w.Store.SweepStale(jobCtx)
	if err != nil {
		slog.Warn("export: воркер: sweep stale", "err", err)
	}
	// SweepStale добивает заявки мимо fail()/failPermanent() — это
	// единственный терминальный исход, о котором Worker.process не узнаёт
	// (заявку с последней попытки уже никто не финализирует, кроме самого
	// SweepStale). Без явного оповещения здесь автор не получил бы письма
	// вовсе, хотя §9 спеки требует его на КАЖДОМ терминальном исходе.
	// job.LastError уже несёт технический текст причины — SweepStale
	// проставляет его той же UPDATE, что переводит заявку в failed (см. её
	// докблок в store.go), отдельная строка здесь не нужна. reasonInternal —
	// для автора инстанс упал посреди работы, различимого повода нет (то же
	// обоснование, что у reasonKey == "" в mailPayload, notify.go).
	for _, job := range swept {
		w.notifyFailed(jobCtx, job, job.LastError, reasonInternal)
	}

	job, ok, err := w.Store.Claim(jobCtx)
	if err != nil {
		return fmt.Errorf("export: воркер: клейм заявки: %w", err)
	}
	if !ok {
		return nil
	}

	w.process(jobCtx, ctx, job)
	return nil
}

// process собирает файл по одной уже заклеймленной заявке и переводит её в
// терминальный статус. Порядок шагов зеркалит §5/§10 спеки: бюджет диска —
// до записи, временный файл — во время, атомарный rename — только после
// успешного закрытия писателя.
//
// runCtx — ctx самого Run(), НЕ ограниченный jobTimeout (в отличие от ctx —
// это jobCtx): единственный признак, по которому process отличает штатную
// остановку процесса (SIGTERM/деплой) от настоящего сбоя сборки. Тому же
// jobCtx.Err() != nil соответствуют ОБА случая (jobCtx наследует отмену от
// runCtx), а runCtx.Err() != nil — только остановка процесса, свой таймаут
// сборки его не трогает (P2-OPS-5).
func (w *Worker) process(ctx, runCtx context.Context, job Job) {
	partPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.part", job.ID))
	finalPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.%s", job.ID, job.Format.Ext()))

	used, err := dirSize(w.Cfg.Dir)
	if err != nil {
		w.fail(ctx, job, fmt.Errorf("подсчёт занятого места в каталоге выгрузок: %w", err), reasonInternal)
		return
	}
	// Бюджет РЕЗЕРВИРУЕТСЯ под текущую заявку (P2-OPS-4 аудита): раньше
	// проверка была used >= DiskBudget, и заявка при used == DiskBudget-1
	// проходила, а затем дописывала до MaxBytes СВЕРХ бюджета — единственный
	// потолок размера файла (Cfg.MaxBytes) её больше не сдерживал, потому что
	// заявку уже пропустили. used+MaxBytes > DiskBudget отказывает раньше:
	// заявка обязана заведомо ПОМЕСТИТЬСЯ в бюджет, а не просто начаться,
	// когда в нём ещё есть хоть один байт.
	if used+w.Cfg.MaxBytes > w.Cfg.DiskBudget {
		w.fail(ctx, job, errors.New("на диске не осталось места под выгрузку: исчерпан общий бюджет каталога"), reasonDiskFull)
		return
	}
	// Реальное свободное место на ФС хоста, а не только сверка с числом из
	// env (P2-OPS-4 аудита): в поставляемом docker-compose pgdata/chdata/
	// exportdata — именованные тома на ОДНОЙ файловой системе, и заявка,
	// уместившаяся в DiskBudget, всё равно может не уместиться на диске,
	// который уже почти съели Postgres/ClickHouse. ok=false (платформа не
	// поддержана freeBytes, см. diskfree_other.go) — проверка пропускается,
	// бюджет остаётся единственным критерием, как было до этой правки.
	if free, ok, err := w.freeBytes(w.Cfg.Dir); err != nil {
		w.fail(ctx, job, fmt.Errorf("подсчёт свободного места на файловой системе: %w", err), reasonInternal)
		return
	} else if ok && free < w.Cfg.MaxBytes {
		w.fail(ctx, job, errors.New("на диске не осталось места под выгрузку: не хватает свободного места на файловой системе хоста"), reasonDiskFull)
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
		case runCtx.Err() != nil:
			// Сборку прервал не отказ, а остановка процесса (SIGTERM/деплой,
			// P2-OPS-5): writeFile/w.stream получили отменённый ctx и вышли с
			// ошибкой отмены, которая иначе попала бы в default ниже и сожгла
			// бы попытку заявки, ни в чём не виноватой. Проверяется ПОСЛЕ
			// permanent-веток нарочно: реальная постоянная ошибка (например,
			// не настроен источник) обязана остаться постоянной ошибкой, даже
			// если она совпала по времени с остановкой процесса — иначе
			// настоящая неисправность конфигурации маскировалась бы под
			// безобидный релиз и молча повторялась бы вечно на каждом старте.
			w.release(ctx, job)
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

	// doneCtx — обычный jobCtx, кроме одного случая: процесс останавливают
	// (runCtx.Err() != nil) ровно в те миллисекунды, когда writeFile уже
	// успел дописать файл (P2-OPS-5). jobCtx в этот момент тоже мёртв (он
	// наследует отмену от runCtx) — без детача Done ушёл бы с уже отменённым
	// ctx и заявка осталась бы 'running' до SweepStale, хотя файл на диске
	// уже полный и годный. Собственный таймаут сборки (jobCtx истёк САМ,
	// runCtx жив) детача НЕ получает и обязан провалить Done как раньше —
	// см. TestWorkerDoesNotFinalizeAfterJobTimeoutExpiresBeforeDone: заявка,
	// перевалившая за свой бюджет времени, не имеет права стать done просто
	// потому что запись в PG случайно оказалась короче отменённого дедлайна.
	doneCtx := ctx
	if runCtx.Err() != nil {
		var cancel context.CancelFunc
		doneCtx, cancel = detachTimeout(ctx)
		defer cancel()
	}
	if err := w.Store.Done(doneCtx, job.ID, job.Attempts, res.rows, res.bytes, res.truncated, w.Cfg.TTL); err != nil {
		// Файл убирается при ЛЮБОЙ ошибке Done, не только ErrStaleClaim:
		// строка так и не станет status='done' (эта попытка либо чужая, либо
		// подтвердить успех не вышло из-за обрыва/таймаута похода в PG), а
		// скачивание требует именно этот статус (internal/web/exports.go) —
		// файл недостижим для автора уже сейчас. И ничто не подберёт его
		// позже: DueForExpiry берёт только status='done' (см. её докблок
		// выше), removeOrphans джанитора считает сиротой файл БЕЗ строки
		// (janitor.go), а строка у этого файла есть — просто не в том
		// статусе. Без явного удаления файл лежит до PurgeRows (30 суток) и
		// всё это время ест GOTCHA_EXPORT_DISK_BUDGET_BYTES. Повторная
		// попытка (если она случится) перепишет файл заново через partPath.
		_ = os.Remove(finalPath)
		if !errors.Is(err, ErrStaleClaim) {
			// ErrStaleClaim — ожидаемая гонка (см. комментарий выше по коду,
			// «Заявку могли удалить...») и не сбой воркера. Любая другая
			// ошибка (обрыв соединения с PG, таймаут пула, истёкший jobCtx)
			// — да, логируем её.
			slog.Warn("export: воркер: завершение заявки", "job_id", job.ID, "err", err)
		}
		return
	}

	if w.Notify != nil {
		done := job
		done.Status = StatusDone
		done.RowsWritten, done.Bytes, done.Truncated = res.rows, res.bytes, res.truncated
		w.Notify(ctx, done)
	}
}

// detachTimeout возвращает контекст с тем же набором значений, что и parent
// (context.WithoutCancel), но полностью отвязанный от его отмены — с
// собственным коротким taймаутом terminalWriteTimeout (P2-OPS-5). Общая
// точка для каждой терминальной записи воркера (fail/failPermanent/
// release, а также Done в process() при остановке процесса): parent (jobCtx)
// умирает вместе с отменой родителя — своим таймаутом сборки или отменой
// Run(ctx) при SIGTERM/деплое, — а запись ИТОГА обязана дойти до PG именно
// тогда, когда сборка уже прервалась, а не только когда всё прошло гладко.
func detachTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), terminalWriteTimeout)
}

// fail помечает заявку временным отказом: попытки ещё есть — вернётся в
// очередь, исчерпаны — станет failed сама Store.Fail. ErrStaleClaim не
// логируется как сбой воркера: лизу потеряли, и это уже не наша забота.
//
// Пишет через detachTimeout(ctx), а не ctx напрямую (P2-OPS-5): ctx —
// jobCtx, к моменту вызова может быть уже мёртв (свой таймаут сборки истёк,
// либо процесс останавливают) — без детача сама запись отказа тоже
// проваливалась бы, и заявка застревала бы в 'running' до SweepStale
// (до 20 минут) вместо немедленного возврата в очередь/отказа.
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
	dctx, cancel := detachTimeout(ctx)
	defer cancel()
	if err := w.Store.Fail(dctx, job.ID, job.Attempts, cause.Error(), reasonKey); err != nil {
		if !errors.Is(err, ErrStaleClaim) {
			slog.Warn("export: воркер: запись неудачи", "job_id", job.ID, "err", err)
		}
		return
	}
	if job.Attempts < maxAttempts {
		return
	}
	w.notifyFailed(dctx, job, cause.Error(), reasonKey)
}

// failPermanent закрывает заявку без права на повтор — причина не устранится
// следующей попыткой (см. ErrPermanent). attempt фенсит владение попыткой
// так же, как в fail/Store.Fail: без него запоздавший постоянный отказ от
// зомби-вызова закрыл бы заявку поверх активной попытки, которая её уже
// переклеймила и продолжает работать. ErrStaleClaim по той же причине не
// логируется как сбой воркера — лизу потеряли, и это уже не наша забота.
//
// Пишет через detachTimeout(ctx) по той же причине, что и fail() — см. её
// докблок (P2-OPS-5).
//
// Notify зовётся сразу — в отличие от fail(), FailPermanent не оставляет
// заявке права на повтор ни на какой попытке, значит первая же и есть
// последняя.
//
// reasonKey — см. докблок fail() выше: тот же смысл, cause здесь уже string,
// а не error.
func (w *Worker) failPermanent(ctx context.Context, job Job, cause string, reasonKey string) {
	dctx, cancel := detachTimeout(ctx)
	defer cancel()
	if err := w.Store.FailPermanent(dctx, job.ID, job.Attempts, cause, reasonKey); err != nil {
		if !errors.Is(err, ErrStaleClaim) {
			slog.Warn("export: воркер: постоянный отказ", "job_id", job.ID, "err", err)
		}
		return
	}
	w.notifyFailed(dctx, job, cause, reasonKey)
}

// release отпускает клейм при штатной остановке процесса (SIGTERM/деплой,
// см. process()/docблок runCtx выше — P2-OPS-5): заявка возвращается в
// очередь тем же attempts, с которым её забрал Claim, — прерванная деплоем
// сборка не вина заявки, жечь на неё попытку из maxAttempts (как это
// сделал бы fail()) неправильно. last_error/failure_reason_key не
// трогаются и Notify не зовётся — это не отказ, автору нечего сообщать.
//
// .part заявки убирает вызывающий (process) сразу после ошибки writeFile,
// той же строкой, что и для остальных веток отказа — ждать stalePartAge
// джанитора незачем, заявка уже вернулась в очередь.
//
// Пишет через detachTimeout(ctx) по той же причине, что и fail() (см. её
// докблок): ctx (jobCtx) в этой ветке уже мёртв — именно отмена родителя
// (runCtx) и привела сюда.
//
// attempt фенсит владение попыткой так же, как Fail/Done/FailPermanent (см.
// их докблоки в store.go): без этого запоздавший релиз от зомби-горутины,
// которая всё ещё досчитывала файл в момент отмены, сбросил бы в очередь
// заявку, которую уже переклеймила и активно строит следующая попытка.
// ErrStaleClaim по той же причине не логируется как сбой воркера — лизу
// потеряли, и это уже не наша забота.
func (w *Worker) release(ctx context.Context, job Job) {
	dctx, cancel := detachTimeout(ctx)
	defer cancel()
	if err := w.Store.Release(dctx, job.ID, job.Attempts); err != nil {
		if !errors.Is(err, ErrStaleClaim) {
			slog.Warn("export: воркер: возврат заявки в очередь при остановке", "job_id", job.ID, "err", err)
		}
	}
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
	// 0o600 — единственное место продукта, где ПДн (user_email/user_ip,
	// contexts, request) ложатся на диск (P3-SEC-1 аудита): 0644 читался бы
	// любым пользователем хоста на bare-metal деплое. os.Rename ниже (см.
	// process()) переносит именно эти права на финальный файл — отдельного
	// os.Chmod для finalPath не нужно.
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
		return w.Issues.Stream(ctx, job.ProjectID, job.IncludePII, job.Params, fn)
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

// DirSize — экспортированная обёртка над dirSize для самометрик (P1-OPS-1,
// gotcha_storage_used_bytes{store="exports"}, см. cmd/gotcha/storagemetrics.go):
// каталог выгрузок — единственный кусок диска, которым распоряжается само
// приложение, и до этой метрики был единственным неизмеряемым.
func DirSize(dir string) (int64, error) { return dirSize(dir) }

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
