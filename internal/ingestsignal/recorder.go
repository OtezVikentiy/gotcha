package ingestsignal

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	// defaultFlushEvery — период тика Recorder.Run по умолчанию.
	defaultFlushEvery = 30 * time.Second
	// defaultMaxPending — потолок числа РАЗНЫХ пар (project_id, kind),
	// копящихся между флашами. Путь Touch — неаутентифицированный (приём
	// зовёт его и на отказ по ключу), поэтому потолок нужен на случай
	// перебора project_id в URL атакующим: без него карта росла бы без
	// границы между тиками.
	defaultMaxPending = 4096
)

// FinalFlushTimeout — бюджет detached-контекста финального Flush при
// остановке процесса (тот же приём, что detachTimeout в internal/export/
// worker.go): ctx.Done() уже наступил, но запись накопленного с последнего
// тика обязана дойти до PG, а не оборваться вместе с ним.
//
// Строго МЕНЬШЕ окна, которым drainIngestSignals (cmd/gotcha/main.go) ждёт
// саму горутину Run — ingestSignalsDrainWindow, 5с (M1/L, финревью волны 1
// аудита перед 1.0): Run() возвращается (и закрывает свой WaitGroup) уже
// ПОСЛЕ того, как этот таймаут истечёт или Flush завершится раньше, так что
// при равных значениях внешнее ожидание при медленной PG гарантированно
// проигрывало бы гонку собственному флашу ещё до того, как Run успеет
// вернуться — drain() уходил бы в Warn, пока Flush ещё в полёте. Экспортирован
// (а не unexported-константа с ручным дублированием числа в cmd/gotcha),
// чтобы TestIngestSignalsFinalFlushFitsDrainWindow сверял это отношение с
// первоисточником, а не с переписанной копией.
const FinalFlushTimeout = 4 * time.Second

// pendingKey — ключ карты накопления Recorder: одна пара (project_id, kind).
type pendingKey struct {
	projectID int64
	kind      Kind
}

// pendingValue — накопленное по одной pendingKey с прошлого Flush.
type pendingValue struct {
	hits     int64
	lastSeen time.Time
}

// Recorder — in-memory аккумулятор сигналов приёма (аудит перед 1.0, находки
// K7-5/K7-6): Touch копит попадания без обращения к БД, Flush сливает
// накопленное через Store.Bump. Аккумулятор, а не запись на каждый Touch —
// путь неаутентифицированный (отказ по ключу считается ДО того, как запрос
// хоть как-то подтверждён), и запись в PG на каждый такой отказ была бы
// усилителем нагрузки: перебор project_id/sentry_key атакующим превращался
// бы в поток INSERT'ов.
type Recorder struct {
	store *Store

	mu      sync.Mutex
	pending map[pendingKey]*pendingValue

	// FlushEvery — период тика Run. Дефолт (30с) можно поменять до первого
	// вызова Run.
	FlushEvery time.Duration
	// MaxPending — потолок РАЗНЫХ пар (project_id, kind) в pending
	// одновременно (см. defaultMaxPending). Дефолт можно поменять до первого
	// Touch.
	MaxPending int

	// dropWarnOnce — предупреждение о переполнении pending пишется один раз
	// за жизнь процесса, тем же приёмом, что и ingest.deprecatedLogged: объём
	// несёт факт, что карта вообще переполнялась, а не счётчик подряд
	// потерянных пар.
	dropWarnOnce sync.Once
}

// NewRecorder создаёт аккумулятор поверх store с дефолтами FlushEvery=30с,
// MaxPending=4096.
func NewRecorder(store *Store) *Recorder {
	return &Recorder{
		store:      store,
		pending:    make(map[pendingKey]*pendingValue),
		FlushEvery: defaultFlushEvery,
		MaxPending: defaultMaxPending,
	}
}

// Touch отмечает одно попадание (projectID, kind) — O(1), без обращения к
// БД. Пара, УЖЕ присутствующая в pending, всегда принимает попадание — потолок
// MaxPending ограничивает только появление НОВЫХ пар: иначе перебор одного и
// того же (живого) project_id перестал бы учитываться раньше, чем перебор
// случайных id, который и должен был отсекать потолок.
func (r *Recorder) Touch(projectID int64, kind Kind) {
	key := pendingKey{projectID: projectID, kind: kind}
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.pending[key]; ok {
		v.hits++
		v.lastSeen = now
		return
	}
	if len(r.pending) >= r.MaxPending {
		r.dropWarnOnce.Do(func() {
			slog.Warn("ingestsignal: recorder: pending map full, new project/kind pairs are dropped until the next flush",
				"max_pending", r.MaxPending)
		})
		return
	}
	r.pending[key] = &pendingValue{hits: 1, lastSeen: now}
}

// Flush снимает pending под мьютексом (следующие Touch копят в свежую пустую
// карту) и пишет каждую пару через Store.Bump. Ошибка одной пары не
// прерывает остальные (errors.Join — как internal/alert.Digest и
// internal/host.Retire). pending НЕ восстанавливается при ошибке: потеря
// счёта на сбое БД допустима — это self-телеметрия, не биллинговый учёт, а
// восстановление пары под мьютексом рядом со свежими Touch усложнило бы
// код ради данных, которым и так место в самотелеметрии, а не в отчётности.
func (r *Recorder) Flush(ctx context.Context) error {
	r.mu.Lock()
	snapshot := r.pending
	r.pending = make(map[pendingKey]*pendingValue)
	r.mu.Unlock()

	var errs error
	for key, v := range snapshot {
		if err := r.store.Bump(ctx, key.projectID, key.kind, v.hits, v.lastSeen); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

// Run крутит тикер FlushEvery до отмены ctx. Ошибка тика логируется и не
// останавливает цикл — следующий тик просто попробует снова (как
// export.Janitor.Run). На ctx.Done() делает финальный Flush detached-
// контекстом с бюджетом FinalFlushTimeout: накопленное с последнего тика не
// должно молча теряться при штатной остановке процесса (SIGTERM/деплой).
func (r *Recorder) Run(ctx context.Context) {
	interval := r.FlushEvery
	if interval <= 0 {
		interval = defaultFlushEvery
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FinalFlushTimeout)
			if err := r.Flush(fctx); err != nil {
				slog.Warn("ingestsignal: recorder: final flush", "err", err)
			}
			cancel()
			return
		case <-ticker.C:
			if err := r.Flush(ctx); err != nil {
				slog.Warn("ingestsignal: recorder: flush", "err", err)
			}
		}
	}
}
