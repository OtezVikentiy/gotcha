package export

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"
)

// snapshotInterval — как часто обновляется снимок очереди выгрузок. Тот же
// приём и тот же интервал, что у notify.Stats (internal/notify/stats.go):
// частоту скрейпа задаёт чужой Prometheus, и сажать запрос к PostgreSQL на
// этот путь значит отдать нагрузку на базу под чужой контроль.
const snapshotInterval = 15 * time.Second

// QueueSnapshot — состояние очереди выгрузок на момент опроса.
type QueueSnapshot struct {
	// Pending — заявок в очереди, ещё не досчитано (queued или running).
	Pending int64
	// Failed — заявок, у которых кончились попытки.
	Failed int64
	// OldestPendingAge — возраст самой старой заявки, ещё не досчитанной.
	//
	// Главное из трёх чисел: только оно отличает «очередь пуста, потому что
	// всё обработано» от «очередь стоит». Ни глубина, ни число провалов
	// этого не показывают — очередь из трёх заявок может быть нормальной
	// работой, а может третьи сутки ждать воркера, который не поднят.
	OldestPendingAge time.Duration
}

// QueueSnapshot — состояние очереди выгрузок: сколько заявок ждёт
// обработки, сколько добито в failed, и сколько ждёт самая старая.
//
// Pending считает queued И running разом: заявка, которую воркер уже
// забрал, но ещё не дописал, с точки зрения дежурного так же "в очереди",
// как и не забранная — до P1-OPS-1 не было видно ни то, ни другое.
// OldestPendingAge берётся от created_at (момент постановки), а не от
// claimed_at: заявка, простоявшая в queued час, а потом обрабатывающаяся
// минуту, всё это время была "заявкой, которую дежурный не увидел".
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

// Stats — наблюдаемость очереди выгрузок для самометрик (P1-OPS-1). До неё
// вставшая очередь и массовые отказы заявок были видны только по тишине в
// логе (Worker пишет только slog.Warn на неудачный тик) — тот же пробел, что
// закрывала notify.Stats (internal/notify/stats.go) для очереди доставки
// уведомлений; этот тип устроен симметрично.
type Stats struct {
	// snapshot хранит *QueueSnapshot: горутина опроса заменяет его целиком,
	// читатели метрик берут без блокировки.
	snapshot atomic.Pointer[QueueSnapshot]
}

// Snapshot — последний снимок очереди. До первого опроса — нули.
func (s *Stats) Snapshot() QueueSnapshot {
	if snap := s.snapshot.Load(); snap != nil {
		return *snap
	}
	return QueueSnapshot{}
}

// Pending, FailedJobs, OldestPendingAgeSeconds — снимок очереди в виде,
// удобном для регистрации метрик (selfmetrics.Registry.AddInt).
func (s *Stats) Pending() int64    { return s.Snapshot().Pending }
func (s *Stats) FailedJobs() int64 { return s.Snapshot().Failed }
func (s *Stats) OldestPendingAgeSeconds() int64 {
	return int64(s.Snapshot().OldestPendingAge / time.Second)
}

// QueueProbe — источник снимков очереди; *Store ему удовлетворяет.
type QueueProbe interface {
	QueueSnapshot(ctx context.Context) (QueueSnapshot, error)
}

// RunSnapshots обновляет снимок очереди, пока не отменят ctx.
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
		// Снимок не обновился — оставляем прежний. Обнулять его нельзя: нули
		// означали бы «очередь пуста», то есть недоступная база выглядела бы
		// как здоровая очередь выгрузок.
		if ctx.Err() == nil {
			slog.Warn("export: queue snapshot failed", "error", err)
		}
		return
	}
	s.snapshot.Store(&snap)
}
