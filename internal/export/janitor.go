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
	defaultJanitorInterval = time.Hour
	// Отдельный от advisoryLockKey воркера — чистка и сборка файлов не конфликтуют.
	janitorLockKey = 0x6578706A
	// Строго больше leaseTTL/jobTimeout сборки: файл живого воркера не может
	// быть настолько стар и одновременно "чужим".
	stalePartAge = time.Hour
	// Дедлайн тика — доля Interval, не меньше пола: иначе повисшая PG-операция
	// держала бы тик бесконечно.
	tickBudgetShare = 0.8
	minTickBudget   = 10 * time.Second
)

// Подчищает файлы-сироты — те, чья строка в export_jobs пропала (каскад
// удаления проекта сносит строки, файлы на диске каскад не задевает).
type Janitor struct {
	Store *Store
	Pool  *pgxpool.Pool
	Dir   string
	// Старше скольких суток от finished_at терминальная строка удаляется вместе с историей.
	RowRetention time.Duration
	// 0 — defaultJanitorInterval.
	Interval time.Duration

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого тика
	lastTickSeconds atomic.Uint64 // длительность последнего тика, math.Float64bits
}

// Self-метрика живости: умерший или зависший джанитор снаружи выглядит как «нечего чистить».
func (j *Janitor) LastTickUnix() int64 { return j.lastTickUnix.Load() }

func (j *Janitor) LastTickSeconds() float64 {
	return math.Float64frombits(j.lastTickSeconds.Load())
}

func (j *Janitor) effectiveInterval() time.Duration {
	if j.Interval <= 0 {
		return defaultJanitorInterval
	}
	return j.Interval
}

// Считается от effectiveInterval, не от сырого Interval — иначе прод (Interval
// не задан) получал бы бюджет minTickBudget вместо ~48 минут.
func (j *Janitor) tickBudget() time.Duration {
	budget := time.Duration(float64(j.effectiveInterval()) * tickBudgetShare)
	if budget < minTickBudget {
		return minTickBudget
	}
	return budget
}

func (j *Janitor) Run(ctx context.Context) {
	ticker := time.NewTicker(j.effectiveInterval())
	defer ticker.Stop()

	// Первый проход сразу, не дожидаясь тика — иначе после рестарта, который
	// случается чаще Interval, диск-бюджет не освобождается до следующего часа.
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

// Порядок шагов обязателен: сироты ищутся после удаления строк (PurgeRows),
// иначе только что осиротевшие файлы ждали бы следующего цикла лишний круг.
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
		// detachTimeout(ctx), не ctx напрямую: снятие лока обязано дойти до PG,
		// даже если ctx тика уже истёк по tickBudget или отменён снаружи.
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

// Ошибка удаления одного файла логируется и не прерывает проход — такая
// заявка останется done с просроченным expires_at и попадёт в DueForExpiry снова.
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

// Свежие .part не трогаются: моложе stalePartAge файл может писать живой
// воркер прямо сейчас.
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
			// Парсинг обязан быть строгим, а не "похоже на число".
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
