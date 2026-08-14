package host

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// touchKey — ключ карты троттлинга Toucher: (project, host).
type touchKey struct {
	projectID int64
	name      string
}

// Toucher троттлит регистрацию хостов на приёме: карта (project,host) →
// время последнего upsert'а, не чаще every на пару, потолок maxEntries с
// вытеснением самой старой записи (иначе кардинальный мусор — случайные или
// поддельные имена хостов — растит карту без границы, в отличие от
// ingest.CardinalityOverflow, который такие имена уже отсеивает выше по
// стеку, но не для ВСЕХ вызывающих).
//
// Горутина на батч, ошибки БД — slog.Warn: Touch вызывается из пути приёма
// событий и не должен ни блокировать его, ни ронять при сбое БД.
type Toucher struct {
	mu    sync.Mutex
	seen  map[touchKey]time.Time
	every time.Duration
	max   int
	// upsert — подменяется в тестах; в бою пишет через Store с таймаутом.
	// Возвращает ошибку, чтобы Touch снял пометку seen и следующий батч
	// попробовал снова (см. Touch).
	upsert func(ctx context.Context, projectID int64, names []string) error
	wg     sync.WaitGroup // для wait() в тестах — дождаться фоновых upsert'ов

	failures atomic.Int64 // проваленные upsert'ы регистрации — self-метрика
	rejected atomic.Int64 // имена, отброшенные потолком MaxHostsPerProject
}

// NewToucher создаёt троттлер поверх store: не чаще every на (project,host),
// не больше maxEntries записей в карте одновременно. store может быть nil —
// тогда дефолтный upsert (подмена ему не задана) молча ничего не делает;
// используется в тестах, которые всегда подменяют tc.upsert напрямую.
func NewToucher(store *Store, every time.Duration, maxEntries int) *Toucher {
	t := &Toucher{
		seen:  make(map[touchKey]time.Time),
		every: every,
		max:   maxEntries,
	}
	t.upsert = func(ctx context.Context, projectID int64, names []string) error {
		if store == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rejected, err := store.Upsert(ctx, projectID, names)
		if err != nil {
			slog.Warn("host: toucher: upsert failed", "project_id", projectID, "error", err)
			return err
		}
		if rejected > 0 {
			// Отказ регистрации обязан быть видимым: без журнала и счётчика
			// «новые машины не появляются в разделе» неотличимо от «они не шлют
			// метрики» — тот же принцип, что у гарда кардинальности.
			t.rejected.Add(int64(rejected))
			slog.Warn("host: toucher: project host limit reached, new host names dropped",
				"project_id", projectID, "dropped", rejected, "limit", MaxHostsPerProject)
		}
		return nil
	}
	return t
}

// UpsertFailures — сколько фоновых upsert'ов регистрации завершились ошибкой
// (self-метрика gotcha_host_registration_failures_total). Растёт, пока
// PostgreSQL недоступен: живость хостов в этот момент не обновляется, и
// оценщик вот-вот откроет ложные silent-инциденты.
func (t *Toucher) UpsertFailures() int64 { return t.failures.Load() }

// RejectedNames — сколько новых имён хостов отброшено потолком
// MaxHostsPerProject (self-метрика gotcha_host_registrations_rejected_total).
func (t *Toucher) RejectedNames() int64 { return t.rejected.Load() }

// Touch регистрирует имена хостов проекта асинхронно и с троттлингом: имена,
// тронутые в пределах every, пропускаются; остальным карта проставляет now(),
// и они уходят в фоновую горутину на upsert. Пустые имена пропускаются
// защитно — фильтрацию по cardinality-переполнению делает вызывающий
// (ingest), чтобы пакет host не зависел от него.
//
// context.WithoutCancel: горутина переживает возврат из Touch и не должна
// обрываться отменой ctx вызывающего (конец обработки батча событий) —
// иначе upsert систематически отменялся бы раньше, чем успевал выполниться.
func (t *Toucher) Touch(ctx context.Context, projectID int64, names []string) {
	if len(names) == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	var due []string
	for _, n := range names {
		if !validName(n) {
			continue
		}
		key := touchKey{projectID: projectID, name: n}
		if last, ok := t.seen[key]; ok {
			if now.Sub(last) < t.every {
				continue
			}
		} else if len(t.seen) >= t.max {
			t.evictOldestLocked()
		}
		t.seen[key] = now
		due = append(due, n)
	}
	upsert := t.upsert
	t.mu.Unlock()

	if len(due) == 0 || upsert == nil {
		return
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		if err := upsert(context.WithoutCancel(ctx), projectID, due); err != nil {
			// Пометка seen ставится ДО upsert'а (он асинхронный), поэтому
			// провалившийся батч обязан её снять: иначе после сбоя PostgreSQL
			// хост считается «тронутым» и не повторяется ещё every — last_seen
			// остаётся старым, и оценщик открывает по живой машине silent.
			// Гонка с параллельным Touch, успевшим пометить те же имена заново,
			// стоит одного лишнего upsert'а — на порядок дешевле ложного алерта.
			t.failures.Add(1)
			for _, n := range due {
				t.Forget(projectID, n)
			}
		}
	}()
}

// evictOldestLocked удаляет запись с наименьшим временем последнего
// upsert'а. Вызывающий обязан держать mu.
func (t *Toucher) evictOldestLocked() {
	var oldestKey touchKey
	var oldestTime time.Time
	first := true
	for k, v := range t.seen {
		if first || v.Before(oldestTime) {
			oldestKey, oldestTime, first = k, v, false
		}
	}
	if !first {
		delete(t.seen, oldestKey)
	}
}

// Forget инвалидирует запись карты для (projectID, name): следующий Touch
// пройдёт немедленно, будто хост ещё не встречался. Нужен при удалении
// хоста (Task 15) — иначе троттлинг помешал бы ему появиться заново, даже
// если он реально снова стал слать события.
func (t *Toucher) Forget(projectID int64, name string) {
	t.mu.Lock()
	delete(t.seen, touchKey{projectID: projectID, name: name})
	t.mu.Unlock()
}

// wait дожидается всех фоновых upsert-горутин, запущенных Touch —
// тестовый хук для детерминизма (в бою никто его не вызывает: Touch
// специально асинхронный, чтобы не тормозить приём событий).
func (t *Toucher) wait() {
	t.wg.Wait()
}
