package ingestsignal

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultFlushEvery = 30 * time.Second
	// Touch — неаутентифицированный путь (приём зовёт его и на отказ по
	// ключу): потолок нужен на случай перебора project_id в URL атакующим.
	defaultMaxPending = 4096
)

// Строго МЕНЬШЕ ingestSignalsDrainWindow (cmd/gotcha/main.go) — иначе drain()
// уйдёт в Warn, пока финальный Flush ещё в полёте.
const FinalFlushTimeout = 4 * time.Second

type pendingKey struct {
	projectID int64
	kind      Kind
}

type pendingValue struct {
	hits     int64
	lastSeen time.Time
}

// Аккумулятор, а не запись на каждый Touch: путь неаутентифицированный, и
// запись в PG на каждый отказ была бы усилителем нагрузки для атакующего.
type Recorder struct {
	store *Store

	mu      sync.Mutex
	pending map[pendingKey]*pendingValue

	// Дефолт можно поменять до первого вызова Run.
	FlushEvery time.Duration
	// Дефолт можно поменять до первого Touch.
	MaxPending int

	dropWarnOnce sync.Once
}

func NewRecorder(store *Store) *Recorder {
	return &Recorder{
		store:      store,
		pending:    make(map[pendingKey]*pendingValue),
		FlushEvery: defaultFlushEvery,
		MaxPending: defaultMaxPending,
	}
}

// Пара, уже присутствующая в pending, всегда принимает попадание — потолок
// MaxPending ограничивает только появление НОВЫХ пар.
func (r *Recorder) Touch(projectID int64, kind Kind) {
	key := pendingKey{projectID: projectID, kind: kind}
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.pending[key]; ok {
		v.hits++
		v.lastSeen = now
		return
	}
	if len(r.pending) >= r.MaxPending {
		r.dropWarnOnce.Do(func() {
			slog.Warn("ingestsignal: recorder: pending map full, new project/kind pairs are dropped until the next flush",
				"max_pending", r.MaxPending)
		})
		return
	}
	r.pending[key] = &pendingValue{hits: 1, lastSeen: now}
}

// pending НЕ восстанавливается при ошибке — потеря счёта на сбое БД
// допустима, это self-телеметрия, а не биллинговый учёт.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	snapshot := r.pending
	r.pending = make(map[pendingKey]*pendingValue)
	r.mu.Unlock()

	var errs error
	for key, v := range snapshot {
		if err := r.store.Bump(ctx, key.projectID, key.kind, v.hits, v.lastSeen); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// На ctx.Done() финальный Flush идёт detached-контекстом с бюджетом
// FinalFlushTimeout — накопленное не должно молча теряться при остановке.
func (r *Recorder) Run(ctx context.Context) {
	interval := r.FlushEvery
	if interval <= 0 {
		interval = defaultFlushEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FinalFlushTimeout)
			if err := r.Flush(fctx); err != nil {
				slog.Warn("ingestsignal: recorder: final flush", "err", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := r.Flush(ctx); err != nil {
				slog.Warn("ingestsignal: recorder: flush", "err", err)
			}
		}
	}
}
