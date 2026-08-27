package uptime

import (
	"context"
	"log/slog"
	"math"
	"sync/atomic"
	"time"
)

// defaultSchedulerEvery — как часто планировщик ставит созревшие проверки в
// очередь. Пять секунд: минимальный интервал монитора — 30 секунд, так что
// задержка постановки на порядок меньше самого частого расписания.
const defaultSchedulerEvery = 5 * time.Second

// scheduleBudget — дедлайн ОДНОГО вызова Svc.Schedule (ревью W3-D: цикл был
// пропущен в записи 1 наравне с Runner/Watchdog — тот же класс дефекта, но
// без метрик и бюджета). `statement_timeout=30с` (internal/db/postgres.go)
// страхует от вечного зависания на уровне PG, но 30 секунд на пятисекундном
// цикле — это шесть пропущенных тиков подряд без единой self-метрики,
// показывающей причину: очередь check_queue не наполняется, монитор
// показан включённым, а «оценщик умер» неотличимо от «созревших проверок
// сейчас нет». Бюджет — та же величина, что у leaseBudget uptime.Runner
// (Svc.LeaseLocal): один простой запрос по индексу, тот же профиль
// нагрузки, что и Svc.Schedule здесь.
const scheduleBudget = 5 * time.Second

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

	lastTickUnix    atomic.Int64  // unix-время последней завершённой постановки
	lastTickSeconds atomic.Uint64 // длительность последней постановки, math.Float64bits
}

// LastTickUnix — unix-время последней завершённой постановки (0, если ни
// одной ещё не было). Self-метрика живости, как у остальных фоновых циклов
// (host.Evaluator и др.): умерший или отставший Scheduler снаружи выглядит
// ровно как «созревших проверок сейчас нет».
func (s *Scheduler) LastTickUnix() int64 { return s.lastTickUnix.Load() }

// LastTickSeconds — длительность последнего вызова Svc.Schedule в секундах.
func (s *Scheduler) LastTickSeconds() float64 {
	return math.Float64frombits(s.lastTickSeconds.Load())
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

// scheduleOnce ограничен дедлайном (scheduleBudget): без него повисший
// Svc.Schedule (голый PG-запрос, statement_timeout=30с — на порядок больше
// пятисекундного цикла) держал бы постановку (и self-метрику живости) до
// самого statement_timeout, шесть тиков подряд, никак не показывая причину.
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
