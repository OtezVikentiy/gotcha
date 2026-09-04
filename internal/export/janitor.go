package export

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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
	// tickBudgetShare/minTickBudget — та же пара, что escalation.Scheduler:
	// дедлайн тика — доля Interval, но не меньше пола, иначе повисшая
	// PG-операция (истечение заявок/чистка истории/сироты) держала бы тик
	// (и self-метрику живости) бесконечно, а следующий тик так и не начался
	// бы.
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
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

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

// LastTickUnix — unix-время последнего завершённого тика (0, если ни одного
// ещё не было). Self-метрика живости, как у escalation.Scheduler: умерший
// или зависший джанитор снаружи выглядит ровно как «нечего чистить».
func (j *Janitor) LastTickUnix() int64 { return j.lastTickUnix.Load() }

// LastTickSeconds — длительность последнего завершённого тика в секундах.
func (j *Janitor) LastTickSeconds() float64 {
	return math.Float64frombits(j.lastTickSeconds.Load())
}

// effectiveInterval — j.Interval с подстановкой дефолта: 0 (не задан, как в
// проде, см. cmd/gotcha/main.go) означает defaultJanitorInterval, а не
// "сразу же" — тот же дефолт, что и Run ниже подставляет тикеру.
func (j *Janitor) effectiveInterval() time.Duration {
	if j.Interval <= 0 {
		return defaultJanitorInterval
	}
	return j.Interval
}

// tickBudget — дедлайн одного тика (см. tickBudgetShare/minTickBudget).
// Считается от effectiveInterval, а не от сырого j.Interval: иначе прод, где
// Interval не задан (Run сам подставляет дефолт), получал бы бюджет
// minTickBudget (10s) вместо ~48 минут — тик обрывался бы на каждой чуть
// более долгой чистке, и диск-бюджет каталога выгрузок никогда не
// освобождался бы до конца.
func (j *Janitor) tickBudget() time.Duration {
	budget := time.Duration(float64(j.effectiveInterval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

// Run крутит тикер до отмены ctx. Ошибка одного тика логируется и не
// останавливает цикл — следующий тик просто попробует снова.
func (j *Janitor) Run(ctx context.Context) {
	ticker := time.NewTicker(j.effectiveInterval())
	defer ticker.Stop()

	// Первый проход — сразу, не дожидаясь тика (как telemetry.EntityJanitor):
	// иначе после каждого рестарта, который случается чаще Interval (час по
	// умолчанию), диск-бюджет каталога выгрузок не освобождается вовсе, а
	// заявки, чей срок истёк ровно перед рестартом, простаивают до
	// следующего часа.
	if err := j.Tick(ctx); err != nil {
		slog.Warn("export: джанитор: тик", "err", err)
	}

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
//
// Tick ограничен дедлайном (tickBudget), как escalation.Scheduler.Tick: без
// внешнего дедлайна повисшая PG-операция держала бы тик (и self-метрику
// живости) бесконечно.
func (j *Janitor) Tick(ctx context.Context) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, j.tickBudget())
	defer cancel()
	defer func() {
		j.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
		if ctx.Err() != nil {
			return
		}
		j.lastTickUnix.Store(time.Now().Unix())
	}()

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
		// detachTimeout(ctx), а не ctx напрямую (K4-5, аудит перед 1.0): к
		// моменту снятия лока ctx тика мог уже истечь по tickBudget (или
		// быть отменён снаружи) — а снятие лока обязано дойти до PG именно
		// тогда, когда сам тик уже не успел, иначе лок доживает до
		// закрытия соединения пулом и блокирует следующий тик до этого
		// момента.
		uctx, cancel := detachTimeout(ctx)
		defer cancel()
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", int64(janitorLockKey)); err != nil {
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
