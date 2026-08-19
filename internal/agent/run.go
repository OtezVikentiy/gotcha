// run.go — цикл сбора и отправки: тик собирает Sample, кодирует в OTLP и либо
// шлёт сразу, либо буферизует, дренируя буфер oldest-first при восстановлении
// связи с инстансом. Один тик — одна отправка «текущего» батча плюс, при её
// успехе, попытка вычистить накопленный буфер.
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

// Границы кольцевого буфера недоставленных батчей (спека §1.3): 120 батчей И
// 8 МиБ суммарно — что раньше упрётся, то и вытесняет старейшее (см. buffer.go).
const (
	bufferMaxBatches = 120
	bufferMaxBytes   = 8 << 20
)

// Экспоненциальный бэкофф между неудачными попытками: 30s·2^(fails-1),
// капается в 10 минут — дольше «тишина» не нужна там, где сервер сам не
// попросил через Retry-After (см. backoffFor).
const (
	backoffBase = 30 * time.Second
	backoffCap  = 10 * time.Minute
)

// maxDrainPerTick — сколько буферизованных батчей выгружаем за один тик
// (ops-MED, thundering herd): без границы после часового простоя дренаж
// одним залпом кладёт 120 запросов подряд, а тики сбора в это время не
// идут — та же горутина занята дренажом. Небольшая порция размазывает
// разгрузку буфера на несколько тиков и не мешает следующему сбору.
const maxDrainPerTick = 8

// runner — состояние цикла отправки между тиками. notBefore/fails реализуют
// бэкофф с полом из Retry-After сервера (429): повторные ошибки не должны
// долбить недоступный/квотированный инстанс на каждом тике.
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
}

// seedFromHost — детерминируемый seed для rand.Rand runner'а: хеш hostname
// (разные хосты парка расходятся по фазе джиттера) в паре с PID (разные
// перезапуски одного хоста тоже не совпадают). Не крипто — джиттеру
// достаточно расхождения фаз, не непредсказуемости.
func seedFromHost(hostname string, pid int) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(hostname))
	return int64(h.Sum64()) ^ int64(pid)
}

// Run запускает цикл сбора-отправки до отмены ctx: раз в cfg.Interval снимает
// Sample, кодирует в OTLP и отправляет либо буферизует. Блокируется — это и
// есть та единственная горутина, которой принадлежит буфер (buffer.go).
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
		hostname:    hostname,
		environment: cfg.Environment,
		role:        cfg.Role,
		collector:   NewCollector(probes),
		sender:      sender,
		buffer:      NewBuffer(bufferMaxBatches, bufferMaxBytes),
		log:         logger,
		rng:         rand.New(rand.NewSource(seedFromHost(hostname, os.Getpid()))),
	}

	// Стартовый баннер (ops-MED, немота): здоровый агент до этой правки не
	// писал в журнал ничего, кроме ошибок — оператор не мог отличить «работает
	// тихо» от «не запустился». Пишем один раз при старте цикла.
	r.log.Info("agent: starting",
		"version", version.Version(),
		"endpoint", cfg.Endpoint,
		"interval", cfg.Interval,
		"hostname", hostname,
	)

	// Первый тик — сразу при старте, а не после ожидания cfg.Interval (до
	// 5 минут, см. maxInterval): свежеустановленный агент не должен молчать
	// на карточке хоста дольше, чем нужно на сам сбор. CPU и так пропускается
	// на этом тике собирателем (нет дельты для первого замера, см. Collector).
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

// tick — один цикл: сбор → (пустой Sample — только лог, без отправки) →
// кодирование → уважение пола бэкоффа → отправка/буферизация.
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
	body, err := EncodeBody(BuildExport(r.hostname, r.environment, r.role, s))
	if err != nil {
		r.log.Error("agent: export encoding failed", "error", err)
		return
	}
	if now.Before(r.notBefore) {
		// пол бэкоффа/Retry-After ещё не истёк — не долбим сервер, копим.
		r.buffer.Push(body)
		return
	}
	r.sendCurrent(ctx, now, body)
}

// sendCurrent отправляет батч текущего тика. SendOK сбрасывает счётчик
// неудач и запускает дренаж накопленного буфера; SendRetry буферизует сам
// этот батч и сдвигает пол следующей попытки; SendDrop — батч отбрасывается
// безвозвратно (повтор не поможет).
func (r *runner) sendCurrent(ctx context.Context, now time.Time, body []byte) {
	result, floor, err := r.sender.Send(ctx, body)
	switch result {
	case SendOK:
		r.fails = 0
		r.noteDelivered()
		r.drain(ctx, now)
		r.noteRecoveredIfDrained()
	case SendRetry:
		r.buffer.Push(body)
		r.backoffAfterFailure(now, floor)
		r.noteBuffering()
		r.log.Warn("agent: send failed, batch buffered", "error", err)
	case SendDrop:
		r.log.Error("agent: batch dropped, no retry", "error", err)
	}
}

// drain выгружает буфер oldest-first, пока отправка проходит успешно, но не
// больше maxDrainPerTick батчей за вызов (ops-MED, thundering herd — см.
// комментарий у константы): остаток разгрузится следующими тиками, а не
// одним залпом, который блокирует сбор в той же горутине.
// SendRetry на дренаже — та же ветка бэкоффа: батч ОСТАЁТСЯ в буфере (без
// DropOldest), fails/notBefore обновляются, и дренаж СРАЗУ прекращается —
// иначе недоступный инстанс получал бы по попытке дренажа на каждом тике
// (ревью плана №11). SendDrop на дренаже — батч отбрасывается с логом,
// дренаж продолжается со следующего: это не отказ доступности, повтор всё
// равно не поможет.
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

// backoffAfterFailure сдвигает пол следующей попытки: не раньше, чем через
// max(floor сервера, экспоненциальный бэкофф по числу подряд неудач), плюс
// джиттер (ops-MED, thundering herd): без него весь парк, потерявший связь
// одновременно (рестарт/обновление инстанса), повторяет попытки в одну и ту
// же секунду по одинаковому детерминированному расписанию.
func (r *runner) backoffAfterFailure(now time.Time, floor time.Duration) {
	r.fails++
	wait := backoffFor(r.fails)
	if floor > wait {
		wait = floor
	}
	wait += jitterBackoff(r.rng, wait)
	r.notBefore = now.Add(wait)
}

// jitterBackoff — случайная добавка к wait в диапазоне [0, min(wait/4,
// backoffCap)]: только увеличивает ожидание, никогда не приближает notBefore
// к «сейчас» относительно базового бэкоффа/пола сервера, и не превышает
// разумного даже при часовом floor из Retry-After (maxRetryWait в sender.go).
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

// noteDelivered логирует факт первой успешной доставки за жизнь процесса
// (ops-MED, немота) — ровно один раз, не на каждом здоровом тике.
func (r *runner) noteDelivered() {
	if r.deliveredOnce {
		return
	}
	r.deliveredOnce = true
	r.log.Info("agent: first batch delivered")
}

// noteBuffering логирует переход в состояние «сервер недоступен, копим
// буфер» — только на смене состояния, не на каждой неудаче (существующие
// per-attempt Warn/Error логи это уже покрывают).
func (r *runner) noteBuffering() {
	if r.buffering {
		return
	}
	r.buffering = true
	r.log.Info("agent: entering buffered mode, server unavailable", "buffered_batches", r.buffer.Len())
}

// noteRecoveredIfDrained логирует восстановление — только когда буфер
// полностью разгружен после того, как агент был в состоянии буферизации
// (иначе строка «recovered» писалась бы на каждом здоровом тике, у которого
// буфер и так пуст).
func (r *runner) noteRecoveredIfDrained() {
	if !r.buffering || r.buffer.Len() != 0 {
		return
	}
	r.buffering = false
	r.log.Info("agent: buffer drained, delivery recovered")
}

// backoffFor — 30s·2^(fails-1), капается в backoffCap.
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

// sampleEmpty — все «содержательные» (карта/срез) секции сбора пусты. Бывает
// при отказе всех content-проб, когда Collect всё же не вернул err благодаря
// успеху скалярных (CPUCount/Load/Uptime/BootTime — см. Collect.ok): экспорт
// из одних нулей отправлять незачем.
func sampleEmpty(s Sample) bool {
	return s.CPU == nil && s.Memory == nil && len(s.Filesystems) == 0 &&
		s.DiskIO == nil && s.NetIO == nil && s.Procs == nil
}
