package notify

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Не «на каждый скрейп»: частоту скрейпа задаёт чужой Prometheus, а фиксированный
// интервал делает нагрузку на БД предсказуемой.
const snapshotInterval = 15 * time.Second

type QueueSnapshot struct {
	Pending int64
	Failed  int64
	// Единственное из трёх чисел, что отличает «очередь пуста, доставлено» от
	// «очередь стоит»: глубина и число провалов сами по себе этого не показывают.
	OldestPendingAge time.Duration
}

// Два вида чисел: счётчики процесса (растут в воркере, бесплатны и точны) и
// снимок очереди (видит и то, что воркер ещё не забирал).
type Stats struct {
	sent    atomic.Int64
	failed  atomic.Int64
	retried atomic.Int64

	// snapshot хранит *QueueSnapshot: горутина опроса заменяет его целиком,
	// читатели метрик берут без блокировки.
	snapshot atomic.Pointer[QueueSnapshot]
}

func (s *Stats) countSent()    { s.sent.Add(1) }
func (s *Stats) countFailed()  { s.failed.Add(1) }
func (s *Stats) countRetried() { s.retried.Add(1) }

func (s *Stats) Sent() int64    { return s.sent.Load() }
func (s *Stats) Failed() int64  { return s.failed.Load() }
func (s *Stats) Retried() int64 { return s.retried.Load() }

// До первого опроса — нули.
func (s *Stats) Snapshot() QueueSnapshot {
	if snap := s.snapshot.Load(); snap != nil {
		return *snap
	}
	return QueueSnapshot{}
}

func (s *Stats) Pending() int64    { return s.Snapshot().Pending }
func (s *Stats) FailedJobs() int64 { return s.Snapshot().Failed }
func (s *Stats) OldestPendingAgeSeconds() int64 {
	return int64(s.Snapshot().OldestPendingAge / time.Second)
}

// *Outbox ему удовлетворяет.
type QueueProbe interface {
	QueueSnapshot(ctx context.Context) (QueueSnapshot, error)
}

func (s *Stats) RunSnapshots(ctx context.Context, probe QueueProbe) {
	ticker := time.NewTicker(snapshotInterval)
	defer ticker.Stop()
	s.refresh(ctx, probe)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx, probe)
		}
	}
}

func (s *Stats) refresh(ctx context.Context, probe QueueProbe) {
	snap, err := probe.QueueSnapshot(ctx)
	if err != nil {
		// Обнулить нельзя: нули читались бы как «очередь пуста», а недоступная
		// база выглядела бы здоровой.
		if ctx.Err() == nil {
			slog.Warn("notify: queue snapshot failed", "error", err)
		}
		return
	}
	s.snapshot.Store(&snap)
}
