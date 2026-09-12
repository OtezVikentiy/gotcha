package host

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type touchKey struct {
	projectID int64
	name      string
}

// Пустая строка значит «неизвестно из этого батча» → «не менять существующее значение», не «сбросить».
// Store.Upsert транслирует это в SQL через NULLIF/CASE — тот же смысл, что дал бы *string == nil.
type TouchEntry struct {
	Name         string
	AgentVersion string
	Environment  string
	Role         string
}

// Потолок maxEntries с вытеснением старейшей — иначе кардинальный мусор растит карту без границы.
// Ошибки БД — только slog.Warn: Touch не должен блокировать или ронять путь приёма событий.
type Toucher struct {
	mu     sync.Mutex
	seen   map[touchKey]time.Time
	every  time.Duration
	max    int
	upsert func(ctx context.Context, projectID int64, entries []TouchEntry) error
	wg     sync.WaitGroup // для wait() в тестах — дождаться фоновых upsert'ов

	failures atomic.Int64 // проваленные upsert'ы регистрации — self-метрика
	rejected atomic.Int64 // имена, отброшенные потолком MaxHostsPerProject
}

// store может быть nil — тогда upsert молча не делает ничего (тесты подменяют tc.upsert напрямую).
func NewToucher(store *Store, every time.Duration, maxEntries int) *Toucher {
	t := &Toucher{
		seen:  make(map[touchKey]time.Time),
		every: every,
		max:   maxEntries,
	}
	t.upsert = func(ctx context.Context, projectID int64, entries []TouchEntry) error {
		if store == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		rejected, err := store.Upsert(ctx, projectID, entries)
		if err != nil {
			slog.Warn("host: toucher: upsert failed", "project_id", projectID, "error", err)
			return err
		}
		if rejected > 0 {
			// Без счётчика/лога «новые машины не появляются» неотличимо от «они не шлют метрики».
			t.rejected.Add(int64(rejected))
			slog.Warn("host: toucher: project host limit reached, new host names dropped",
				"project_id", projectID, "dropped", rejected, "limit", MaxHostsPerProject)
		}
		return nil
	}
	return t
}

// Метрика gotcha_host_registration_failures_total.
// Растёт, пока PostgreSQL недоступен — оценщик рискует открыть ложный silent по живым хостам.
func (t *Toucher) UpsertFailures() int64 { return t.failures.Load() }

// Метрика gotcha_host_registrations_rejected_total.
func (t *Toucher) RejectedNames() int64 { return t.rejected.Load() }

// Троттлинг ключуется только по имени: смена версии внутри every не форсирует upsert.
// context.WithoutCancel — горутина переживает возврат из Touch, иначе upsert отменится раньше срока.
func (t *Toucher) Touch(ctx context.Context, projectID int64, entries []TouchEntry) {
	if len(entries) == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	var due []TouchEntry
	for _, e := range entries {
		if !validName(e.Name) {
			continue
		}
		key := touchKey{projectID: projectID, name: e.Name}
		if last, ok := t.seen[key]; ok {
			if now.Sub(last) < t.every {
				continue
			}
		} else if len(t.seen) >= t.max {
			t.evictOldestLocked()
		}
		t.seen[key] = now
		due = append(due, e)
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
			// seen ставится ДО upsert (асинхронного) — провал обязан снять её, иначе last_seen зависает и
			// оценщик откроет ложный silent по живому хосту; гонка с параллельным Touch стоит лишнего upsert.
			t.failures.Add(1)
			for _, e := range due {
				t.Forget(projectID, e.Name)
			}
		}
	}()
}

// Вызывающий обязан держать mu — сама функция не блокирует.
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

// Нужен при удалении хоста — иначе троттлинг мешал бы ему появиться заново, даже реально ожив.
func (t *Toucher) Forget(projectID int64, name string) {
	t.mu.Lock()
	delete(t.seen, touchKey{projectID: projectID, name: name})
	t.mu.Unlock()
}

// Тестовый хук: в бою никто не вызывает — Touch намеренно асинхронный, чтобы не тормозить приём.
func (t *Toucher) wait() {
	t.wg.Wait()
}
