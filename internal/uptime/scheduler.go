package uptime

import (
	"context"
	"log/slog"
	"time"
)

// defaultSchedulerEvery — как часто планировщик ставит созревшие проверки в
// очередь. Пять секунд: минимальный интервал монитора — 30 секунд, так что
// задержка постановки на порядок меньше самого частого расписания.
const defaultSchedulerEvery = 5 * time.Second

// Scheduler ставит созревшие проверки в очередь (check_queue). Только ставит —
// исполняют их Runner (локальный регион) и выносные пробы через /probe/lease.
//
// Живёт отдельно от Runner намеренно. Раньше постановка была вторым тикером
// внутри Runner, а Runner собирается только в режимах uptime и all — и это
// значило, что в документированном раздельном развёртывании web+ingest очередь
// не наполнялась НИКОГДА. Оператор заводил монитор, тот показывался
// включённым, страница монитора рисовала «нет данных», состояние оставалось
// unknown, пропуски heartbeat и истечение сертификатов не считались вовсе — и
// ни одной строки в логе. Выносные пробы в таком развёртывании тоже
// простаивали: они честно опрашивали пустую очередь.
//
// Постановка идемпотентна (UNIQUE(monitor_id, region) + ON CONFLICT DO
// NOTHING) и двигает last_scheduled_at только у тех мониторов, чьё задание
// реально вставилось, поэтому несколько реплик с планировщиком друг другу не
// мешают и расписание не растягивают.
type Scheduler struct {
	Svc *Service

	// Every — период постановки; 0 означает defaultSchedulerEvery.
	Every time.Duration
}

// Run ставит проверки в очередь до отмены контекста.
func (s *Scheduler) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = defaultSchedulerEvery
	}
	tick := time.NewTicker(every)
	defer tick.Stop()

	// Первая постановка — сразу: иначе после рестарта проверки простаивают до
	// первого тика, а у монитора с интервалом в 30 секунд это заметная доля
	// его расписания.
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
	if _, err := s.Svc.Schedule(ctx); err != nil {
		slog.Error("uptime: scheduler: schedule failed", "error", err)
	}
}
