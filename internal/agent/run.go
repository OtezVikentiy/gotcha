package agent

import (
	"context"
	"hash/fnv"
	"log/slog"
	"math/rand"
	"os"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

// 120 батчей И 8 МиБ суммарно — что раньше упрётся, то и вытесняет старейшее.
const (
	bufferMaxBatches = 120
	bufferMaxBytes   = 8 << 20
)

// 30s·2^(fails-1), капается в 10 минут — дольше без Retry-After сервера
// не нужно.
const (
	backoffBase = 30 * time.Second
	backoffCap  = 10 * time.Minute
)

// Ограничивает выгрузку буфера за тик — без него дренаж после долгого
// простоя блокирует сбор одним 120-запросным залпом.
const maxDrainPerTick = 8

// notBefore/fails — бэкофф с полом из Retry-After сервера (429).
type runner struct {
	hostname    string
	environment string // resource-метка deployment.environment; "" — не эмитится
	role        string // resource-метка host.role; "" — не эмитится
	collector   *Collector
	sender      *Sender
	buffer      *Buffer
	log         *slog.Logger
	rng         *rand.Rand // джиттер бэкоффа (thundering herd), seed — см. seedFromHost

	notBefore time.Time
	fails     int

	deliveredOnce bool // первая успешная доставка уже залогирована
	buffering     bool // сейчас в состоянии «сервер недоступен, копим буфер» (для логов перехода)

	dropped          int64 // точек за всё время жизни процесса — едет сервером как DroppedPointsMetric
	droppedUndrained int   // точек с момента, когда буфер последний раз опустел, — для честности «recovered»

	startedAtNano uint64 // StartTimeUnixNano для DroppedPointsMetric — с запуска процесса, не хоста
}

// Хеш hostname расходится по хосту, PID — по рестарту одного хоста; не
// крипто, джиттеру нужно только расхождение фаз.
func seedFromHost(hostname string, pid int) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(hostname))
	return int64(h.Sum64()) ^ int64(pid)
}

// Блокируется — единственная горутина, которой принадлежит buffer.go.
func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	sender, err := NewSender(cfg)
	if err != nil {
		return err
	}
	hostname := cfg.Hostname
	if hostname == "" {
		h, err := os.Hostname()
		if err != nil {
			return err
		}
		hostname = h
	}
	probes := DefaultProbes()
	probes.Procs = throttledProcs(probes.Procs, time.Now, procsProbeInterval)
	r := &runner{
		hostname:      hostname,
		environment:   cfg.Environment,
		role:          cfg.Role,
		collector:     NewCollector(probes),
		sender:        sender,
		buffer:        NewBuffer(bufferMaxBatches, bufferMaxBytes),
		log:           logger,
		rng:           rand.New(rand.NewSource(seedFromHost(hostname, os.Getpid()))),
		startedAtNano: uint64(time.Now().UnixNano()),
	}

	// Один раз при старте — иначе оператор не отличит «работает тихо» от
	// «не запустился».
	r.log.Info("agent: starting",
		"version", version.Version(),
		"endpoint", cfg.Endpoint,
		"interval", cfg.Interval,
		"hostname", hostname,
	)

	// Сразу при старте, не после ожидания interval — свежий агент не должен
	// молчать на карточке хоста дольше, чем нужно на сам сбор.
	r.tick(ctx, time.Now())

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			r.tick(ctx, now)
		}
	}
}

func (r *runner) tick(ctx context.Context, now time.Time) {
	s, err := r.collector.Collect(now)
	if err != nil {
		r.log.Error("agent: metrics collection failed", "error", err)
		return
	}
	if sampleEmpty(s) {
		r.log.Warn("agent: all content probes empty for this tick, skipping")
		return
	}
	md := BuildExport(r.hostname, r.environment, r.role, s, r.dropped, r.startedAtNano)
	points := exportPointCount(md)
	body, err := EncodeBody(md)
	if err != nil {
		r.log.Error("agent: export encoding failed", "error", err)
		return
	}
	if now.Before(r.notBefore) {
		// пол бэкоффа/Retry-After ещё не истёк — не долбим сервер, копим.
		r.pushBuffered(body, points)
		return
	}
	r.sendCurrent(ctx, now, body, points)
}

// Единственный путь класть батч в буфер — переполнение считается и логируется
// с причиной, а не проглатывается молча.
func (r *runner) pushBuffered(body []byte, points int) {
	r.buffer.Push(body, points)
	oversized, evicted := r.buffer.TakeDropped()
	if oversized > 0 {
		r.dropped += int64(oversized)
		r.droppedUndrained += oversized
		r.log.Error("agent: batch dropped, exceeds buffer size limit",
			"points", oversized, "total_dropped_points", r.dropped)
	}
	if evicted > 0 {
		r.dropped += int64(evicted)
		r.droppedUndrained += evicted
		r.log.Error("agent: batch dropped, buffer full",
			"points", evicted, "total_dropped_points", r.dropped)
	}
}

func (r *runner) sendCurrent(ctx context.Context, now time.Time, body []byte, points int) {
	result, floor, err := r.sender.Send(ctx, body)
	switch result {
	case SendOK:
		r.fails = 0
		r.noteDelivered()
		r.drain(ctx, now)
		r.noteRecoveredIfDrained()
	case SendRetry:
		r.pushBuffered(body, points)
		r.backoffAfterFailure(now, floor)
		r.noteBuffering()
		r.log.Warn("agent: send failed, batch buffered", "error", err)
	case SendDrop:
		r.log.Error("agent: batch dropped, no retry", "error", err)
	}
}

// SendRetry обрывает дренаж сразу — иначе недоступный инстанс получал бы
// попытку на каждом тике. SendDrop продолжает со следующего батча.
func (r *runner) drain(ctx context.Context, now time.Time) {
	for i := 0; i < maxDrainPerTick; i++ {
		body, ok := r.buffer.Oldest()
		if !ok {
			return
		}
		result, floor, err := r.sender.Send(ctx, body)
		switch result {
		case SendOK:
			r.buffer.DropOldest()
		case SendRetry:
			r.backoffAfterFailure(now, floor)
			r.noteBuffering()
			r.log.Warn("agent: drain interrupted, batch remains buffered", "error", err)
			return
		case SendDrop:
			r.log.Error("agent: drain batch dropped, no retry", "error", err)
			r.buffer.DropOldest()
		}
	}
}

// Пол — max(floor сервера, бэкофф по числу неудач) + джиттер: без него весь
// парк, потерявший связь разом, повторяет попытки в одну секунду.
func (r *runner) backoffAfterFailure(now time.Time, floor time.Duration) {
	r.fails++
	wait := backoffFor(r.fails)
	if floor > wait {
		wait = floor
	}
	wait += jitterBackoff(r.rng, wait)
	r.notBefore = now.Add(wait)
}

// [0, min(wait/4, backoffCap)] — только увеличивает ожидание, никогда не
// приближает notBefore к «сейчас».
func jitterBackoff(rng *rand.Rand, wait time.Duration) time.Duration {
	if wait <= 0 || rng == nil {
		return 0
	}
	ceil := wait / 4
	if ceil > backoffCap {
		ceil = backoffCap
	}
	if ceil <= 0 {
		return 0
	}
	return time.Duration(rng.Int63n(int64(ceil) + 1))
}

// Только один раз за жизнь процесса, не на каждом здоровом тике.
func (r *runner) noteDelivered() {
	if r.deliveredOnce {
		return
	}
	r.deliveredOnce = true
	r.log.Info("agent: first batch delivered")
}

// Только на смене состояния — per-attempt Warn/Error логи уже покрывают
// остальное.
func (r *runner) noteBuffering() {
	if r.buffering {
		return
	}
	r.buffering = true
	r.log.Info("agent: entering buffered mode, server unavailable", "buffered_batches", r.buffer.Len())
}

// Только после буферизации и при пустом буфере — иначе «recovered» писалось
// бы на каждом здоровом тике.
func (r *runner) noteRecoveredIfDrained() {
	if !r.buffering || r.buffer.Len() != 0 {
		return
	}
	r.buffering = false
	lost := r.droppedUndrained
	r.droppedUndrained = 0
	if lost > 0 {
		r.log.Warn("agent: buffer drained, delivery resumed with data loss", "dropped_points", lost)
		return
	}
	r.log.Info("agent: buffer drained, delivery recovered")
}

func backoffFor(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	shift := fails - 1
	const maxShift = 20 // 30s<<20 уже на порядки больше capа — дальше сдвигать незачем
	if shift > maxShift {
		shift = maxShift
	}
	d := backoffBase << uint(shift)
	if d <= 0 || d > backoffCap { // d<=0 — переполнение при большом сдвиге
		return backoffCap
	}
	return d
}

// Бывает при отказе всех content-проб, когда Collect всё же не вернул err
// благодаря успеху скалярных проб (CPUCount/Load/Uptime/BootTime).
func sampleEmpty(s Sample) bool {
	return s.CPU == nil && s.Memory == nil && len(s.Filesystems) == 0 &&
		s.DiskIO == nil && s.NetIO == nil && s.Procs == nil
}
