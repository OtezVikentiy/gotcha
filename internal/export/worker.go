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
	defaultTickInterval = 5 * time.Second
	// Строго меньше leaseTTL: иначе второй инстанс переклеймит заявку, которую первый ещё пишет.
	defaultJobTimeout = 15 * time.Minute
	// "expo" в ASCII — под локом только одна реплика, вторая молча уступает проход.
	advisoryLockKey = 0x6578706F
	// Бюджет на запись итога в PG — не даёт зомби-записи зависнуть, если БД недоступна при остановке.
	terminalWriteTimeout = 5 * time.Second
)

func init() {
	if defaultJobTimeout >= leaseTTL {
		panic("export: defaultJobTimeout обязан быть строго меньше leaseTTL")
	}
}

// Для причин, которые повтор не устранит — ошибки конфигурации и данных, не transient сбои.
var ErrPermanent = errors.New("export: постоянный отказ сборки выгрузки")

// Внутренний сентинел: наружу не выходит, process разбирает его сам.
var errLimitReached = errors.New("export: достигнут потолок заявки")

// Технический текст (last_error) остаётся техническим — для письма автору идёт только reasonKey,
// один из трёх готовых переводов, не сама cause-строка.
const (
	reasonDiskFull      = "exports.mail.failed.reason.disk_full"
	reasonTooManyGroups = "exports.mail.failed.reason.too_many_groups"
	reasonInternal      = "exports.mail.failed.reason.internal"
)

// i18n.T на неизвестном ключе возвращает сам ключ, не перевод — без проверки повреждённый
// или устаревший ключ дошёл бы до пользователя техническим текстом напрямую.
func KnownFailureReasonKey(key string) bool {
	switch key {
	case reasonDiskFull, reasonTooManyGroups, reasonInternal:
		return true
	}
	return false
}

// Экспортирован для guards.TestExportFailureReasonKeysResolve: ключ приходит в i18n.T() переменной,
// общий сканер каталога переводов такой вызов не ловит.
var FailureReasonKeys = []string{reasonDiskFull, reasonTooManyGroups, reasonInternal}

type Config struct {
	Dir      string
	TTL      time.Duration
	MaxRows  int64
	MaxBytes int64
	// Переполнение — постоянный отказ, не частично записанный файл.
	DiskBudget   int64
	TickInterval time.Duration
	// 0 — defaultJobTimeout; Validate() требует JobTimeout < leaseTTL при любом заданном значении.
	JobTimeout time.Duration
}

func (c Config) tickInterval() time.Duration {
	if c.TickInterval > 0 {
		return c.TickInterval
	}
	return defaultTickInterval
}

func (c Config) jobTimeout() time.Duration {
	if c.JobTimeout > 0 {
		return c.JobTimeout
	}
	return defaultJobTimeout
}

// 0 не значит «без лимита» здесь (в отличие от GOTCHA_DIST_RATE_PER_MIN/*_RETENTION_DAYS) —
// тихо включает усечение по защитному пределу без Truncated=true.
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

type Worker struct {
	Store  *Store
	Pool   *pgxpool.Pool
	Issues IssueSource
	Events EventSource
	Cfg    Config
	// nil допустим — тестам почта не нужна.
	Notify func(context.Context, Job)
	// nil — используется боевая platformFreeBytes; поле существует, чтобы тесты подменяли значение,
	// не завися от реального свободного места на диске, где гоняются тесты.
	FreeBytes func(dir string) (free int64, ok bool, err error)
}

func (w *Worker) freeBytes(dir string) (int64, bool, error) {
	if w.FreeBytes != nil {
		return w.FreeBytes(dir)
	}
	return freeBytes(dir)
}

// Ошибка тика не останавливает воркер — уже осела в заявке или логе, следующий тик пробует снова.
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

// Ошибка — сбой тика, не заявки: неудача сборки уже осела в Fail/FailPermanent, наружу не всплывает.
func (w *Worker) Tick(ctx context.Context) error {
	if err := w.Cfg.Validate(); err != nil {
		return fmt.Errorf("export: воркер: %w", err)
	}

	conn, err := w.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("export: воркер: получение соединения: %w", err)
	}
	defer conn.Release()

	// Лок сессионный — на явном соединении: через пул он мог бы уйти на другое, лок бы не держался.
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(advisoryLockKey)).Scan(&locked); err != nil {
		return fmt.Errorf("export: воркер: advisory lock: %w", err)
	}
	if !locked {
		// Другая реплика уже ведёт проход — не сбой.
		return nil
	}
	defer func() {
		// detachTimeout, не ctx: при остановке процесса ctx уже отменён — без детача снятие лока падало бы
		// с ошибкой на каждом деплое и приучало бы игнорировать реальные предупреждения.
		uctx, cancel := detachTimeout(ctx)
		defer cancel()
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", int64(advisoryLockKey)); err != nil {
			slog.Warn("export: воркер: снятие advisory lock", "err", err)
		}
	}()

	// jobCtx покрывает шаги вплоть до сборки — Background() был бы зомби-воркер, дописывающий файл
	// после штатной остановки; терминальная запись итога — исключение, идёт через detachTimeout().
	jobCtx, cancel := context.WithTimeout(ctx, w.Cfg.jobTimeout())
	defer cancel()

	swept, err := w.Store.SweepStale(jobCtx)
	if err != nil {
		slog.Warn("export: воркер: sweep stale", "err", err)
	}
	// SweepStale финализирует заявки мимо fail()/failPermanent() — без явного notifyFailed здесь
	// автор не получил бы письма о провале вовсе.
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

// runCtx (Run(), не ограничен jobTimeout) — единственный признак штатной остановки: jobCtx.Err()
// истинен и при отмене, и при таймауте сборки, а runCtx.Err() — только при остановке процесса.
func (w *Worker) process(ctx, runCtx context.Context, job Job) {
	partPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.part", job.ID))
	finalPath := filepath.Join(w.Cfg.Dir, fmt.Sprintf("%d.%s", job.ID, job.Format.Ext()))

	used, err := dirSize(w.Cfg.Dir)
	if err != nil {
		w.fail(ctx, job, fmt.Errorf("подсчёт занятого места в каталоге выгрузок: %w", err), reasonInternal)
		return
	}
	// Резервируем под заявку: used+MaxBytes > DiskBudget, не used >= DiskBudget —
	// иначе заявка при почти пустом остатке дописала бы сверх бюджета до MaxBytes.
	if used+w.Cfg.MaxBytes > w.Cfg.DiskBudget {
		w.fail(ctx, job, errors.New("на диске не осталось места под выгрузку: исчерпан общий бюджет каталога"), reasonDiskFull)
		return
	}
	// Реальное свободное место на ФС хоста — DiskBudget не видит, что pgdata/chdata делят с exportdata
	// одну ФС; ok=false (платформа не поддержана) — бюджет остаётся единственным критерием.
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
			// Единственная причина, которую автор может устранить сам (сузить фильтр) — отдельный ключ письма.
			w.failPermanent(ctx, job, err.Error(), reasonTooManyGroups)
		case errors.Is(err, ErrPermanent) || errors.Is(err, ErrMaxIssueIDsNotConfigured):
			w.failPermanent(ctx, job, err.Error(), reasonInternal)
		case runCtx.Err() != nil:
			// Остановка процесса, не отказ — writeFile получил отменённый ctx. Проверка ПОСЛЕ permanent-веток:
			// постоянная ошибка обязана остаться постоянной, даже совпав по времени с остановкой.
			w.release(ctx, job)
		default:
			w.fail(ctx, job, err, reasonInternal)
		}
		return
	}

	// Заявку могли удалить, пока писался файл: Done ниже фенсит по id+status+attempts как Fail —
	// удалённая строка даёт те же 0 строк → ErrStaleClaim, отдельно перепроверять не нужно.
	if err := os.Rename(partPath, finalPath); err != nil {
		_ = os.Remove(partPath)
		w.fail(ctx, job, fmt.Errorf("переименование файла выгрузки: %w", err), reasonInternal)
		return
	}

	// doneCtx детачится ТОЛЬКО если процесс останавливают (runCtx.Err()!=nil) — jobCtx тоже мёртв,
	// без этого Done ушёл бы с отменённым ctx. Истёкший СВОЙ таймаут сборки детача не получает.
	doneCtx := ctx
	if runCtx.Err() != nil {
		var cancel context.CancelFunc
		doneCtx, cancel = detachTimeout(ctx)
		defer cancel()
	}
	if err := w.Store.Done(doneCtx, job.ID, job.Attempts, res.rows, res.bytes, res.truncated, w.Cfg.TTL); err != nil {
		// Файл убирается при ЛЮБОЙ ошибке Done, не только ErrStaleClaim — без 'done' статуса он недостижим
		// для скачивания, и ничто его не подберёт без явного удаления (DueForExpiry берёт только done).
		_ = os.Remove(finalPath)
		if !errors.Is(err, ErrStaleClaim) {
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

// Контекст с значениями parent (WithoutCancel), но отвязанный от его отмены — короткий свой таймаут.
// Общая точка терминальных записей: parent может быть мёртв, а итог обязан дойти до PG.
func detachTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), terminalWriteTimeout)
}

// Пишет через detachTimeout: jobCtx может быть уже мёртв, иначе и запись отказа провалилась бы.
// Notify — только при исчерпанных попытках, иначе спам письмами на каждой временной неудаче.
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

// В отличие от fail(), Notify зовётся сразу — FailPermanent не оставляет заявке права на повтор.
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

// Возвращает заявку с тем же attempts — прерванная деплоем сборка не вина заявки, попытка не горит.
// last_error/reason_key не трогаются, Notify не зовётся — это не отказ.
func (w *Worker) release(ctx context.Context, job Job) {
	dctx, cancel := detachTimeout(ctx)
	defer cancel()
	if err := w.Store.Release(dctx, job.ID, job.Attempts); err != nil {
		if !errors.Is(err, ErrStaleClaim) {
			slog.Warn("export: воркер: возврат заявки в очередь при остановке", "job_id", job.ID, "err", err)
		}
	}
}

// FailureReasonKey тут проставляет notifyFailed, а не store.go — не персистится, нужно только снимку.
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

type writeResult struct {
	rows      int64
	bytes     int64
	truncated bool
}

// По ошибке файл остаётся не переименованным — удаление .part на совести вызывающего (process).
func (w *Worker) writeFile(ctx context.Context, job Job, partPath string) (writeResult, error) {
	// 0o600 — единственное место, где ПДн (email/ip/contexts/request) ложится на диск: 0644
	// читался бы любым пользователем хоста на bare-metal. os.Rename переносит те же права на финальный файл.
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
		// Потолок строк — ДО записи: обрезать ровно на MaxRows строке, не на MaxRows+1.
		if w.Cfg.MaxRows > 0 && res.rows >= w.Cfg.MaxRows {
			res.truncated = true
			return errLimitReached
		}
		if err := wr.Write(rec); err != nil {
			return fmt.Errorf("запись строки выгрузки: %w", err)
		}
		res.rows++
		// Потолок байт — ПОСЛЕ записи: размер строки заранее не известен, но она попадает в файл.
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

// ScopeIssueID — только у событий: у групп область всегда «проект целиком с фильтром».
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
		// IncludePII — снимок заявки: один w.Events обслуживает заявки с разным значением по очереди.
		return w.Events.Stream(ctx, job.ProjectID, job.ScopeIssueID, job.IncludePII, job.Params, fn)
	default:
		return fmt.Errorf("%w: неизвестный вид выгрузки %q", ErrPermanent, job.Kind)
	}
}

func columnsFor(k Kind) []string {
	if k == KindEvents {
		return EventColumns()
	}
	return IssueColumns()
}

func DirSize(dir string) (int64, error) { return dirSize(dir) }

// Без рекурсии: в каталоге только .part и готовые файлы, подкаталогов не бывает.
func dirSize(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	return sizeOfEntries(entries)
}

// os.IsNotExist(err) пропускается, не валит process(): файл мог убрать параллельный джанитор между
// os.ReadDir и Info() — бюджет отказывает только когда РЕАЛЬНО не осталось места.
func sizeOfEntries(entries []os.DirEntry) (int64, error) {
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

// Считает реальный размер файла (BOM/скобки/разделители), а не только сумму значений колонок.
type byteCounter struct {
	w io.Writer
	n int64
}

func (c *byteCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
