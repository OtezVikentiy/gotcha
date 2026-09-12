package export

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// Частоту скрейпа задаёт внешний Prometheus — запрос к PostgreSQL на этот путь садить нельзя.
const snapshotInterval = 15 * time.Second

type QueueSnapshot struct {
	Pending int64
	Failed  int64
	// Только это поле отличает «очередь пуста» от «очередь стоит» — глубина и Failed не покажут.
	OldestPendingAge time.Duration
}

// Pending считает queued И running — заявка у воркера всё ещё «в очереди» с точки зрения дежурного.
// OldestPendingAge — от created_at, не claimed_at: время без внимания дежурного, не обработки.
func (s *Store) QueueSnapshot(ctx context.Context) (QueueSnapshot, error) {
	var snap QueueSnapshot
	var oldestSecs float64
	err := s.pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE status IN ('queued', 'running')),
			count(*) FILTER (WHERE status = 'failed'),
			coalesce(extract(epoch FROM now() - min(created_at) FILTER (WHERE status IN ('queued', 'running'))), 0)
		FROM export_jobs`).Scan(&snap.Pending, &snap.Failed, &oldestSecs)
	if err != nil {
		return QueueSnapshot{}, fmt.Errorf("export: снимок очереди: %w", err)
	}
	if oldestSecs > 0 {
		snap.OldestPendingAge = time.Duration(oldestSecs * float64(time.Second))
	}
	return snap, nil
}

type Stats struct {
	// Заменяется горутиной опроса целиком, читатели берут без блокировки.
	snapshot atomic.Pointer[QueueSnapshot]
}

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
		// При ошибке прежний снимок не обнуляем: нули означали бы «пусто», база выглядела бы здоровой.
		if ctx.Err() == nil {
			slog.Warn("export: queue snapshot failed", "error", err)
		}
		return
	}
	s.snapshot.Store(&snap)
}
