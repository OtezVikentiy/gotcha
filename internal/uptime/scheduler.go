package uptime

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"
)

// 5с: минимальный интервал монитора — 30с, задержка постановки на порядок
// меньше самого частого расписания.
const defaultSchedulerEvery = 5 * time.Second

// без бюджета повисший Schedule держал бы self-метрику живости мёртвой до
// statement_timeout=30с — шесть тиков без единого сигнала о причине.
const scheduleBudget = 5 * time.Second

// только ставит задания, исполняют Runner и выносные пробы через /probe/lease;
// идемпотентна (UNIQUE(monitor_id, region) + ON CONFLICT DO NOTHING).
type Scheduler struct {
	Svc *Service

	// Every — период постановки; 0 означает defaultSchedulerEvery.
	Every time.Duration

	lastTickUnix    atomic.Int64  // unix-время последней завершённой постановки
	lastTickSeconds atomic.Uint64 // длительность последней постановки, math.Float64bits
}

// 0, если ни одной постановки ещё не было. Мёртвый или отставший Scheduler
// снаружи выглядит как «созревших проверок сейчас нет».
func (s *Scheduler) LastTickUnix() int64 { return s.lastTickUnix.Load() }

func (s *Scheduler) LastTickSeconds() float64 {
	return math.Float64frombits(s.lastTickSeconds.Load())
}

func (s *Scheduler) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = defaultSchedulerEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()

	// первая постановка сразу — иначе после рестарта монитор с 30-секундным
	// интервалом простаивает заметную долю расписания до первого тика.
	s.scheduleOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.scheduleOnce(ctx)
		}
	}
}

func (s *Scheduler) scheduleOnce(ctx context.Context) {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, scheduleBudget)
	defer cancel()

	_, err := s.Svc.Schedule(ctx)

	s.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if err != nil {
		slog.Error("uptime: scheduler: schedule failed", "error", err)
		return
	}
	s.lastTickUnix.Store(time.Now().Unix())
}
