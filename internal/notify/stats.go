package notify

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// snapshotInterval — как часто обновляется снимок очереди.
//
// 15 секунд, а не «на каждый скрейп /metrics»: частоту скрейпа задаёт чужой
// Prometheus, и ставить запрос к PostgreSQL на путь, дёргаемый с неизвестной
// частотой, значит отдать нагрузку на базу под чужой контроль. Постоянный
// интервал делает эту нагрузку известной.
const snapshotInterval = 15 * time.Second

// QueueSnapshot — состояние очереди доставки на момент опроса.
type QueueSnapshot struct {
	// Pending — задач ждёт отправки.
	Pending int64
	// Failed — задач, у которых кончились попытки.
	Failed int64
	// OldestPendingAge — возраст самой старой ждущей задачи.
	//
	// Главное из трёх чисел: только оно отличает «очередь пуста, потому что всё
	// доставлено» от «очередь стоит». Ни глубина, ни число провалов этого не
	// показывают — очередь из трёх задач может быть нормальной работой, а может
	// стоять третьи сутки.
	OldestPendingAge time.Duration
}

// Stats — наблюдаемость доставки уведомлений.
//
// До неё «алерт не пришёл» диагностировался грепом логов, причём janitor через
// семь дней удалял улику. Числа делятся на два вида: счётчики процесса (растут
// в воркере, бесплатны и точны) и снимок очереди (видит и то, чего воркер не
// забирал — очередь могла встать до него).
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

// Sent, Failed, Retried — доставки за время жизни процесса.
func (s *Stats) Sent() int64    { return s.sent.Load() }
func (s *Stats) Failed() int64  { return s.failed.Load() }
func (s *Stats) Retried() int64 { return s.retried.Load() }

// Snapshot — последний снимок очереди. До первого опроса — нули.
func (s *Stats) Snapshot() QueueSnapshot {
	if snap := s.snapshot.Load(); snap != nil {
		return *snap
	}
	return QueueSnapshot{}
}

// Pending, FailedJobs, OldestPendingAgeSeconds — снимок очереди в виде,
// удобном для регистрации метрик.
func (s *Stats) Pending() int64    { return s.Snapshot().Pending }
func (s *Stats) FailedJobs() int64 { return s.Snapshot().Failed }
func (s *Stats) OldestPendingAgeSeconds() int64 {
	return int64(s.Snapshot().OldestPendingAge / time.Second)
}

// QueueProbe — источник снимков очереди; *Outbox ему удовлетворяет.
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
		// как здоровая доставка.
		if ctx.Err() == nil {
			slog.Warn("notify: queue snapshot failed", "error", err)
		}
		return
	}
	s.snapshot.Store(&snap)
}
