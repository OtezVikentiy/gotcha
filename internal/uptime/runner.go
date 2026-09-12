package uptime

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const DefaultRegion = "local"

const (
	defaultLeaseEvery  = time.Second
	defaultConcurrency = 50
)

// дедлайн ОДНОГО LeaseLocal, не всей раздачи по семафору (это backpressure,
// не зависание); 5с — на порядок больше обычного запроса, но не транзиент.
const leaseBudget = 5 * time.Second

// постановку заданий Runner не делает — она в Scheduler, который может
// работать в другом процессе (раздельное развёртывание web+ingest).
type Runner struct {
	Svc    *Service
	Writer *ResultWriter

	Region      string
	Concurrency int

	// по умолчанию (false) SSRF-фильтр включён: чекеры режут
	// loopback/приватные/link-local адреса.
	AllowPrivateTargets bool

	LeaseEvery time.Duration

	// переопределение CheckerFor по Kind для тестов; nil — используется
	// пакетный CheckerFor.
	Checkers map[Kind]Checker

	// колбэк после ApplyResult; nil — не делает ничего.
	OnResult func(ctx context.Context, m Monitor, region string, r Result, st State)

	wg sync.WaitGroup // проверки, выполняющиеся прямо сейчас (см. Close)

	initOnce sync.Once
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	// собирается в init() из Svc/Writer/OnResult, чтобы Runner можно было
	// собрать литералом без конструктора.
	ing *Ingestor

	lastTickUnix    atomic.Int64  // unix-время последнего завершённого lease-прохода
	lastTickSeconds atomic.Uint64 // длительность последнего lease-прохода, math.Float64bits
}

// 0, если ни одного прохода ещё не было. Мёртвый или отставший Runner
// снаружи выглядит как «мониторов к проверке сейчас нет».
func (r *Runner) LastTickUnix() int64 { return r.lastTickUnix.Load() }

// только сам вызов LeaseLocal — раздача по семафору исполнителям не входит.
func (r *Runner) LastTickSeconds() float64 {
	return math.Float64frombits(r.lastTickSeconds.Load())
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

func (r *Runner) checkerFor(kind Kind) (Checker, error) {
	if c, ok := r.Checkers[kind]; ok {
		return c, nil
	}
	return CheckerFor(kind, r.AllowPrivateTargets)
}

// если пул семафора занят, тик блокируется — осознанный backpressure:
// планировщик чуть задержится, но не переполнит очередь горутинами.
func (r *Runner) Run(ctx context.Context) {
	r.init()
	defer close(r.done)

	leaseEvery := r.LeaseEvery
	if leaseEvery <= 0 {
		leaseEvery = defaultLeaseEvery
	}

	leaseTick := time.NewTicker(leaseEvery)
	defer leaseTick.Stop()

	sem := make(chan struct{}, r.concurrency())

	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-leaseTick.C:
			r.leaseAndDispatch(ctx, sem)
		}
	}
}

// блокировка на занятом семафоре ограничена этим тиком, не всем Run —
// раздача продолжится, когда освободится слот.
func (r *Runner) leaseAndDispatch(ctx context.Context, sem chan struct{}) {
	started := time.Now()
	leaseCtx, cancel := context.WithTimeout(ctx, leaseBudget)
	jobs, err := r.Svc.LeaseLocal(leaseCtx, r.region(), r.concurrency())
	cancel()
	r.lastTickSeconds.Store(math.Float64bits(time.Since(started).Seconds()))
	if err != nil {
		slog.Error("uptime: runner: lease failed", "region", r.region(), "error", err)
		return
	}
	r.lastTickUnix.Store(time.Now().Unix())
	for _, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		case <-r.stop:
			// незавершённая раздача при Close не теряет задания — они остаются
			// зализенными в БД и переберутся заново после истечения лизы.
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

// паника внутри чекера ловится и становится Result{OK:false} — иначе она
// уронила бы весь процесс, который в --mode=all держит ещё web+ingest.
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
		return checkWithRetries(ctx, checker, j.Monitor)
	}()
	at := time.Now().UTC()

	// отмена ctx при SIGTERM — не падение: иначе каждый деплой открывал бы
	// инциденты по context canceled; задание перевыполнится после истечения лизы.
	if ctx.Err() != nil && !result.OK {
		slog.Info("uptime: runner: check aborted by shutdown, result discarded",
			"monitor_id", j.MonitorID, "region", j.Region, "queue_id", j.QueueID)
		return
	}

	// пишем через context.WithoutCancel: ctx уже отменён в проде к моменту
	// Close (см. drain() в main.go), но таймаут не даёт зависнуть при недоступной БД.
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := r.ing.Accept(dbCtx, j, at, result); err != nil {
		slog.Error("uptime: runner: accept result failed",
			"monitor_id", j.MonitorID, "region", j.Region, "queue_id", j.QueueID, "error", err)
	}
}

// идемпотентен; не зависит от ctx, переданного в Run. Вызов без хотя бы
// одного запущенного Run заблокируется навсегда — ждать нечего.
func (r *Runner) Close() {
	r.init()
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
	r.wg.Wait()
}
