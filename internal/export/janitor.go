package export

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// defaultJanitorInterval — период тика Janitor.Run по умолчанию, как у
	// notify.OutboxJanitor: срок хранения измеряется часами/сутками, и
	// заглядывать чаще незачем.
	defaultJanitorInterval = time.Hour
	// janitorLockKey — произвольный, но постоянный ключ сессионного advisory
	// lock прохода джанитора. Отдельный от advisoryLockKey воркера (store.go
	// "expo"): им незачем делить один лок, чистка и сборка файлов не
	// конфликтуют друг с другом. "expj" в ASCII.
	janitorLockKey = 0x6578706A
	// stalePartAge — .part-файл старше этого возраста в каталоге выгрузок
	// считается мусором упавшего инстанса, а не файлом, который прямо сейчас
	// пишет живой воркер. Строго больше leaseTTL/jobTimeout сборки: файл
	// живого воркера не может быть настолько стар и одновременно
	// "чужим" — либо лиза уже протухла и заявку переклеймили, либо процесс
	// действительно упал.
	stalePartAge = time.Hour
)

// Janitor чистит после себя очередь выгрузок: убирает файлы и строки
// заявок, чей срок хранения истёк, чистит историю старше RowRetention и
// подчищает файлы-сироты — те, чья строка в export_jobs пропала (каскад
// удаления проекта сносит строки, файлы на диске каскад не задевает).
type Janitor struct {
	Store *Store
	Pool  *pgxpool.Pool
	// Dir — каталог выгрузок, тот же, что у Worker.Cfg.Dir.
	Dir string
	// RowRetention — старше скольких суток от finished_at терминальная
	// строка удаляется вместе с историей (Store.PurgeRows).
	RowRetention time.Duration
	// Interval — период тика; 0 — defaultJanitorInterval.
	Interval time.Duration
}

// Run крутит тикер до отмены ctx. Ошибка одного тика логируется и не
// останавливает цикл — следующий тик просто попробует снова.
func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = defaultJanitorInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := j.Tick(ctx); err != nil {
				slog.Warn("export: джанитор: тик", "err", err)
			}
		}
	}
}

// Tick выполняет один проход тремя шагами: истёкшие файлы+заявки, старые
// строки истории, файлы-сироты. Порядок обязателен — сироты ищутся ПОСЛЕ
// удаления строк (PurgeRows), иначе только что осиротевшие файлы
// (строку снёс этот же тик) ждали бы следующего цикла лишний круг.
func (j *Janitor) Tick(ctx context.Context) error {
	conn, err := j.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("export: джанитор: получение соединения: %w", err)
	}
	defer conn.Release()

	// Лок сессионный и берётся на явном соединении: через пул без него
	// каждый QueryRow мог бы уйти на другое соединение и лок бы не держался.
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(janitorLockKey)).Scan(&locked); err != nil {
		return fmt.Errorf("export: джанитор: advisory lock: %w", err)
	}
	if !locked {
		// Проход идёт на другой реплике — это нормальная работа, не сбой.
		return nil
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", int64(janitorLockKey)); err != nil {
			slog.Warn("export: джанитор: снятие advisory lock", "err", err)
		}
	}()

	if err := j.expireDue(ctx); err != nil {
		return err
	}
	if _, err := j.Store.PurgeRows(ctx, j.RowRetention); err != nil {
		return fmt.Errorf("export: джанитор: чистка старых заявок: %w", err)
	}
	if err := j.removeOrphans(ctx); err != nil {
		return err
	}
	return nil
}

// expireDue удаляет файлы заявок, чей срок хранения истёк, и переводит их
// строки в expired. Ошибка удаления одного файла логируется и не прерывает
// проход — иначе один битый файл заморозил бы уборку остальных; такая
// заявка просто останется done с просроченным expires_at и попадёт в
// DueForExpiry на следующем тике.
func (j *Janitor) expireDue(ctx context.Context) error {
	jobs, err := j.Store.DueForExpiry(ctx)
	if err != nil {
		return fmt.Errorf("export: джанитор: заявки на истечение срока: %w", err)
	}
	if len(jobs) == 0 {
		return nil
	}

	expired := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		path := filepath.Join(j.Dir, fmt.Sprintf("%d.%s", job.ID, job.FileExt))
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("export: джанитор: удаление файла истёкшей заявки", "job_id", job.ID, "err", err)
			continue
		}
		expired = append(expired, job.ID)
	}
	if err := j.Store.MarkExpired(ctx, expired); err != nil {
		return fmt.Errorf("export: джанитор: пометка истёкших заявок: %w", err)
	}
	return nil
}

// removeOrphans чистит каталог выгрузок от файлов без строки в export_jobs
// (строку снесли PurgeRows или каскад удаления проекта, файл остался) и от
// протухших .part — мусора упавшего на записи инстанса.
//
// Свежие .part не трогаются: моложе stalePartAge файл может писать живой
// воркер прямо сейчас, снести его значило бы разъехаться с активной
// сборкой.
func (j *Janitor) removeOrphans(ctx context.Context) error {
	entries, err := os.ReadDir(j.Dir)
	if err != nil {
		return fmt.Errorf("export: джанитор: чтение каталога выгрузок: %w", err)
	}

	type file struct {
		name string
		id   int64
	}
	var candidates []file
	var ids []int64
	seen := make(map[int64]bool)

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.TrimPrefix(filepath.Ext(name), ".")
		if ext == "" {
			// Имя без расширения — не наш файл (все выгрузки и .part пишутся
			// как "<id>.<ext>"), не трогаем.
			continue
		}
		base := strings.TrimSuffix(name, "."+ext)
		id, err := strconv.ParseInt(base, 10, 64)
		if err != nil || id <= 0 {
			// Имя не в строгом формате <id>.<ext> с положительным id — не
			// наш файл, не трогаем: парсинг обязан быть строгим, а не
			// "похоже на число".
			continue
		}

		if ext == "part" {
			info, err := e.Info()
			if err != nil {
				slog.Warn("export: джанитор: stat временного файла", "name", name, "err", err)
				continue
			}
			if time.Since(info.ModTime()) < stalePartAge {
				continue
			}
			if err := os.Remove(filepath.Join(j.Dir, name)); err != nil && !os.IsNotExist(err) {
				slog.Warn("export: джанитор: удаление протухшего .part", "name", name, "err", err)
			}
			continue
		}

		candidates = append(candidates, file{name: name, id: id})
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	if len(candidates) == 0 {
		return nil
	}
	existing, err := j.Store.ExistingIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("export: джанитор: проверка сирот: %w", err)
	}
	for _, c := range candidates {
		if existing[c.id] {
			continue
		}
		if err := os.Remove(filepath.Join(j.Dir, c.name)); err != nil && !os.IsNotExist(err) {
			slog.Warn("export: джанитор: удаление файла-сироты", "name", c.name, "err", err)
		}
	}
	return nil
}
