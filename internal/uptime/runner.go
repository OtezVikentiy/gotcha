package uptime

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultRegion — встроенный регион локальной пробы (та же реплика, что и
// центральный процесс). Существует всегда, даже без единой выносной пробы
// (см. Service.Regions и спека §4).
const DefaultRegion = "local"

const (
	defaultScheduleEvery = 5 * time.Second
	defaultLeaseEvery    = time.Second
	defaultConcurrency   = 50
)

// Runner — локальная проба: исполняет проверки в том же процессе, что и
// центр (регион DefaultRegion, если Region не задан). Два тикера —
// планировщик (ScheduleEvery, ставит задания через Svc.Schedule) и
// исполнитель (LeaseEvery, забирает задания через Svc.LeaseLocal и
// выполняет их пулом, ограниченным Concurrency). Zero-value полей
// (ScheduleEvery/LeaseEvery/Concurrency/Region) означает "используй
// дефолт" — так Runner можно собрать литералом без конструктора, как
// notify.Worker/alert.Spike.
type Runner struct {
	Svc    *Service
	Writer *ResultWriter

	Region      string
	Concurrency int

	// AllowPrivateTargets отключает SSRF-фильтр приватных целей в HTTP/TCP
	// чекерах (прокидывается в CheckerFor). false (по умолчанию) — фильтр
	// включён: чекеры режут loopback/приватные/link-local адреса.
	AllowPrivateTargets bool

	ScheduleEvery time.Duration
	LeaseEvery    time.Duration

	// Checkers — опциональное переопределение CheckerFor по Kind, для
	// тестов (инъекция фейкового чекера, например паникующего). nil
	// (по умолчанию) — используется пакетный CheckerFor.
	Checkers map[Kind]Checker

	// OnResult — опциональный колбэк после ApplyResult; nil (по умолчанию)
	// — ничего не делает. План 3 подключит сюда детекцию инцидентов.
	OnResult func(ctx context.Context, m Monitor, region string, r Result, st State)

	wg sync.WaitGroup // проверки, выполняющиеся прямо сейчас (см. Close)

	initOnce sync.Once
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	// ing — общий с /probe/results хвост обработки результата (см.
	// Ingestor); собирается в init() из Svc/Writer/OnResult, чтобы Runner
	// по-прежнему можно было собрать литералом без конструктора.
	ing *Ingestor
}

func (r *Runner) init() {
	r.initOnce.Do(func() {
		r.stop = make(chan struct{})
		r.done = make(chan struct{})
		r.ing = &Ingestor{Svc: r.Svc, Writer: r.Writer, OnResult: r.OnResult}
	})
}

func (r *Runner) region() string {
	if r.Region == "" {
		return DefaultRegion
	}
	return r.Region
}

func (r *Runner) concurrency() int {
	if r.Concurrency <= 0 {
		return defaultConcurrency
	}
	return r.Concurrency
}

// checkerFor resolves the Checker for kind — r.Checkers[kind] if the test
// injected one, otherwise the package-level CheckerFor.
func (r *Runner) checkerFor(kind Kind) (Checker, error) {
	if c, ok := r.Checkers[kind]; ok {
		return c, nil
	}
	return CheckerFor(kind, r.AllowPrivateTargets)
}

// Run — цикл планировщика+исполнителя; запускать горутиной. Завершается,
// когда отменяется ctx или зовётся Close. Каждый тик исполнителя блокируется
// на семафоре, если пул занят, — это осознанное давление назад
// (backpressure): следующий тик планировщика чуть задержится, но очередь
// не переполнится необработанными горутинами.
func (r *Runner) Run(ctx context.Context) {
	r.init()
	defer close(r.done)

	scheduleEvery := r.ScheduleEvery
	if scheduleEvery <= 0 {
		scheduleEvery = defaultScheduleEvery
	}
	leaseEvery := r.LeaseEvery
	if leaseEvery <= 0 {
		leaseEvery = defaultLeaseEvery
	}

	scheduleTick := time.NewTicker(scheduleEvery)
	defer scheduleTick.Stop()
	leaseTick := time.NewTicker(leaseEvery)
	defer leaseTick.Stop()

	sem := make(chan struct{}, r.concurrency())

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-scheduleTick.C:
			if _, err := r.Svc.Schedule(ctx); err != nil {
				slog.Error("uptime: runner: schedule failed", "error", err)
			}
		case <-leaseTick.C:
			r.leaseAndDispatch(ctx, sem)
		}
	}
}

// leaseAndDispatch забирает до Concurrency готовых заданий своего региона и
// запускает их проверку в пуле, ограниченном sem. Занятые слоты семафора
// блокируют раздачу следующих заданий этого тика (не всего Run — только
// текущего вызова), пока какая-то проверка не освободит слот.
func (r *Runner) leaseAndDispatch(ctx context.Context, sem chan struct{}) {
	jobs, err := r.Svc.LeaseLocal(ctx, r.region(), r.concurrency())
	if err != nil {
		slog.Error("uptime: runner: lease failed", "region", r.region(), "error", err)
		return
	}
	for _, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		case <-r.stop:
			// Close was called while this tick was still handing out
			// leased jobs to a full pool: stop dispatching immediately
			// rather than waiting for a worker to free up. The jobs not
			// yet dispatched stay leased in the DB and get retried once
			// their lease expires — same as any other DB-side skip.
			return
		}
		r.wg.Add(1)
		go func(j Job) {
			defer r.wg.Done()
			defer func() { <-sem }()
			r.runOne(ctx, j)
		}(j)
	}
}

// runOne выполняет одно задание: чекер → Ingestor.Accept (ClaimJob → буфер CH
// → ApplyResult → OnResult — тот же хвост, что и у результатов выносных проб,
// см. ingest.go). Ошибка самого чекера (сайт лежит, DNS не резолвится и т.п.)
// — не Go-ошибка, а нормальный Result{OK:false}, который доходит до Accept как
// обычно. Проверка, чей lease успел протухнуть и чьё задание перехватила
// другая реплика, свой результат не применит: claim не пройдёт (см.
// Ingestor.Accept). Ошибка похода в БД — лог и возврат. Паника
// внутри самого чекера (баг в стороннем коде проверки) перехватывается и
// превращается в Result{OK:false} — иначе она уронила бы весь процесс,
// который в --mode=all держит ещё и web+ingest.
func (r *Runner) runOne(ctx context.Context, j Job) {
	checker, err := r.checkerFor(j.Monitor.Kind)
	if err != nil {
		slog.Error("uptime: runner: no checker for job", "monitor_id", j.MonitorID, "kind", j.Monitor.Kind, "error", err)
		return
	}

	result := func() (res Result) {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("uptime: runner: checker panicked", "monitor_id", j.MonitorID, "kind", j.Monitor.Kind, "panic", p)
				res = Result{OK: false, Error: "internal checker panic"}
			}
		}()
		return checker.Check(ctx, j.Monitor)
	}()
	at := time.Now().UTC()

	// The check itself uses ctx (so a shutdown aborts a slow in-flight HTTP
	// check quickly), but everything after this point is a fire-once DB
	// write that must not be lost just because ctx was cancelled: Close()
	// waits for exactly these calls (see Close's doc comment) via r.wg, and
	// in production ctx is ALREADY cancelled by the time Close() runs
	// (cmd/gotcha/main.go's drain() calls Close() only after the run ctx is
	// done). context.WithoutCancel keeps request-scoped values but drops
	// cancellation, and the bounded timeout still lets these calls give up
	// instead of hanging forever if the DB is unreachable.
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := r.ing.Accept(dbCtx, j, at, result); err != nil {
		slog.Error("uptime: runner: accept result failed",
			"monitor_id", j.MonitorID, "region", j.Region, "queue_id", j.QueueID, "error", err)
	}
}

// Close останавливает цикл Run и дожидается завершения проверок,
// выполняющихся прямо сейчас (включая их ClaimJob/ApplyResult).
// Идемпотентен — повторный вызов безопасен. Не зависит от ctx, переданного
// в Run: если тот уже отменён, Close всё равно корректно дождётся выхода
// из цикла и in-flight проверок через собственный канал остановки (как
// ResultWriter.Close, вызывать Close без хотя бы одного запущенного Run —
// заблокируется навсегда, ждать нечего).
func (r *Runner) Close() {
	r.init()
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	r.wg.Wait()
}
