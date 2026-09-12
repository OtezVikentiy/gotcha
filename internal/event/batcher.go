package event

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"

	"gitflic.ru/otezvikentiy/gotcha/internal/chbatch"
)

// Столько подряд-фейлов вставки одного батча терпим, прежде чем перейти к
// изоляции ядовитых рядов бинарным дроблением (chbatch.IsolatePoison).
const poisonThreshold = 3

type Conn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// Ошибка вставки возвращает пачку в буфер для ретрая следующим тиком.
// Буфер ограничен maxBuf — при переполнении дропается самое старое.
type Batcher struct {
	conn Conn

	mu          sync.Mutex
	buf         []Event
	bufBytes    int64 // приблизительный вес buf, см. maxBufBytes
	dropped     int64
	insertFails int64 // накопительно: сколько флашей провалилось
	failStreak  int
	lastDropLog time.Time
	// nil, пока дропов нет — не аллоцируется на горячем пути. Заполняется в
	// trimLocked под mu, сливается в onDrop вне mu (emitDrops).
	pendingDrops map[int64]int64
	// Сток per-org дропов буфера; nil — no-op (например, в тестах без пайплайна).
	// Читается и пишется под mu.
	onDrop func(orgID, n int64)

	maxBuf      int
	maxBufBytes int64
	batchSize   int
	interval    time.Duration

	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewBatcher(conn Conn) *Batcher {
	return &Batcher{
		conn:        conn,
		maxBuf:      10000,
		maxBufBytes: defaultMaxBufBytes,
		batchSize:   1000,
		interval:    5 * time.Second,
		kick:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Не блокирует и не возвращает ошибку — приём событий не должен зависеть от
// здоровья ClickHouse.
func (b *Batcher) Add(ev Event) {
	size := eventBytes(ev)
	b.mu.Lock()
	b.buf = append(b.buf, ev)
	b.bufBytes += size
	logDrop := b.trimLocked()
	dropped := b.dropped
	full := len(b.buf) >= b.batchSize
	if logDrop && time.Since(b.lastDropLog) > b.interval {
		b.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	// Захватываем per-org дропы и сток в той же критической секции; сам вызов
	// стока — ВНЕ mu (сток берёт свой мьютекс, держать оба нельзя).
	drops, sink := b.takeDropsLocked()
	b.mu.Unlock()
	reportDrops(sink, drops)
	if logDrop {
		slog.Warn("event buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

// Ставится один раз до горячего трафика; nil-сток — no-op.
func (b *Batcher) SetDropSink(fn func(orgID, n int64)) {
	b.mu.Lock()
	b.onDrop = fn
	b.mu.Unlock()
}

// Вызывается под mu; вызывающий сливает результат через reportDrops после разблокировки.
func (b *Batcher) takeDropsLocked() (map[int64]int64, func(orgID, n int64)) {
	if len(b.pendingDrops) == 0 {
		return nil, b.onDrop
	}
	m := b.pendingDrops
	b.pendingDrops = nil
	return m, b.onDrop
}

// Для путей, где дроп мог случиться под mu без прямого возврата стока (flush).
func (b *Batcher) emitDrops() {
	b.mu.Lock()
	drops, sink := b.takeDropsLocked()
	b.mu.Unlock()
	reportDrops(sink, drops)
}

func reportDrops(sink func(orgID, n int64), drops map[int64]int64) {
	if sink == nil {
		return
	}
	for orgID, n := range drops {
		sink(orgID, n)
	}
}

func (b *Batcher) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Растущая глубина буфера — первый признак, что хранилище не принимает.
func (b *Batcher) Buffered() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.buf))
}

// Отличается от Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется, потеря наступает только при переполнении буфера.
func (b *Batcher) InsertFailures() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.insertFails
}

// Максимум по двум потолкам (строки и байты) — упереться в один уже насыщение.
// Не обрезается единицей: буфер может физически перебрать потолок между append и trimLocked.
func (b *Batcher) Saturation() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := bufSaturation(int64(len(b.buf)), int64(b.maxBuf))
	bytes := bufSaturation(b.bufBytes, b.maxBufBytes)
	if bytes > rows {
		return bytes
	}
	return rows
}

// den<=0 — потолок выключен нулём («не ограничены»), не «делить не на что».
func bufSaturation(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// Даже без собственного дедлайна у parent ctx: сетевой чёрный дыр в
// PrepareBatch/Send не должен вешать Run/Close навсегда.
func (b *Batcher) flushWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	b.flush(ctx)
}

// Запускать горутиной; завершается через Close.
func (b *Batcher) Run() {
	defer close(b.done)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ticker.C:
			b.flushWithTimeout(context.Background())
		case <-b.kick:
			b.flushWithTimeout(context.Background())
		}
	}
}

// При неудачных вставках ретраит с паузой, пока жив ctx, каждая попытка
// ограничена внутренним таймаутом. Идемпотентен — повторный вызов безопасен.
func (b *Batcher) Close(ctx context.Context) error {
	b.stopOnce.Do(func() { close(b.stop) })
	<-b.done
	err := b.closeDrain(ctx)
	if dropped := b.Dropped(); dropped > 0 {
		slog.Warn("events dropped during lifetime", "dropped_total", dropped)
	}
	return err
}

func (b *Batcher) closeDrain(ctx context.Context) error {
	for {
		b.mu.Lock()
		n := len(b.buf)
		b.mu.Unlock()
		if n == 0 {
			return nil
		}
		b.flushWithTimeout(ctx)
		b.mu.Lock()
		left := len(b.buf)
		b.mu.Unlock()
		if left == 0 {
			return nil
		}
		if left >= n { // флаш не продвинулся — пауза перед ретраем
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

// Строка события может доходить до ~1 МиБ — maxBuf=10000 таких строк это больше 10 ГБ.
const defaultMaxBufBytes = 256 << 20

// Без rowOverheadBytes байтовый потолок обходится строками из пустых значений —
// они весили бы почти ноль, и потолок срабатывал бы только по числу строк.
const rowOverheadBytes = 64

func eventBytes(ev Event) int64 {
	n := len(ev.ID) + len(ev.Level) + len(ev.Message) +
		len(ev.ExceptionType) + len(ev.ExceptionValue) + len(ev.Stacktrace) +
		len(ev.Environment) + len(ev.Release) + len(ev.ServerName) + len(ev.SDK) +
		len(ev.UserID) + len(ev.UserIP) + len(ev.UserEmail) +
		len(ev.Contexts) + len(ev.Breadcrumbs) + len(ev.Request) +
		len(ev.TraceID) + len(ev.SpanID)
	for k, v := range ev.Tags {
		n += len(k) + len(v)
	}
	return int64(n) + rowOverheadBytes
}

// Стоимость — O(числа выброшенных), не O(len(buf)): вес буфера ведётся
// инкрементально в Add. Вызывается под mu.
func (b *Batcher) trimLocked() bool {
	drop := 0
	if over := len(b.buf) - b.maxBuf; over > 0 {
		drop = over
		for i := 0; i < over; i++ {
			b.bufBytes -= eventBytes(b.buf[i])
		}
	}
	// Одну строку оставляем всегда: событие тяжелее потолка само по себе не
	// повод отдать буфер пустым — оно уйдёт ближайшим флашем.
	for drop < len(b.buf)-1 && b.bufBytes > b.maxBufBytes {
		b.bufBytes -= eventBytes(b.buf[drop])
		drop++
	}
	if drop <= 0 {
		return false
	}
	// Списываем выброшенные строки их организациям — иначе потеря на этом
	// слое не видна per-org.
	for i := 0; i < drop; i++ {
		if org := b.buf[i].OrgID; org > 0 {
			if b.pendingDrops == nil {
				b.pendingDrops = make(map[int64]int64)
			}
			b.pendingDrops[org]++
		}
	}
	b.buf = append(b.buf[:0], b.buf[drop:]...)
	b.dropped += int64(drop)
	return true
}

// Нужен там, где буфер перестраивается целиком (возврат пачки), а не растёт
// по одному событию. Вызывается под mu.
func (b *Batcher) recountLocked() {
	b.bufBytes = 0
	for i := range b.buf {
		b.bufBytes += eventBytes(b.buf[i])
	}
}

func (b *Batcher) flush(ctx context.Context) {
	// Возврат провалившейся пачки может переполнить буфер и вызвать trimLocked —
	// сливаем накопленные per-org дропы после того, как flush отпустит mu.
	defer b.emitDrops()
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return
	}
	n := len(b.buf)
	if n > b.batchSize {
		n = b.batchSize
	}
	batch := make([]Event, n)
	copy(batch, b.buf[:n])
	b.buf = append(b.buf[:0], b.buf[n:]...)
	b.recountLocked()
	b.mu.Unlock()

	if err := b.insert(ctx, batch); err != nil {
		// Data-level «яд» изолируем сразу, транзиент терпим до порога и лишь потом эскалируем.
		poison := chbatch.IsServerDataError(err)
		b.mu.Lock()
		b.failStreak++
		streak := b.failStreak
		b.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved. Дополняет per-value UUID-фолбэк в insert.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, b.insert, chbatch.IsServerDataError)
			b.mu.Lock()
			b.dropped += int64(dropped)
			b.insertFails++
			// Сбрасываем только если изоляция что-то разрешила — иначе при
			// лежащем CH дробление перезапускается каждые ~15с без толку.
			if dropped > 0 || len(unresolved) < len(batch) {
				b.failStreak = 0
			}
			var over int
			if len(unresolved) > 0 {
				b.buf = append(unresolved, b.buf...)
				before := b.dropped
				b.recountLocked()
				b.trimLocked()
				over = int(b.dropped - before)
			}
			b.mu.Unlock()
			if dropped > 0 || over > 0 {
				slog.Warn("event batch: isolated poison rows",
					"dropped", dropped, "unresolved", len(unresolved), "overflow", over, "batch", len(batch))
			}
			return
		}

		b.mu.Lock()
		b.buf = append(batch, b.buf...)
		before := b.dropped
		b.recountLocked()
		b.trimLocked()
		over := int(b.dropped - before)
		b.insertFails++
		b.mu.Unlock()
		slog.Warn("event batch insert failed, will retry",
			"events", len(batch), "error", err, "dropped", over)
		return
	}
	b.mu.Lock()
	b.failStreak = 0
	b.mu.Unlock()
}

func (b *Batcher) insert(ctx context.Context, events []Event) error {
	// Колонки перечислены явно: безымянный INSERT ломается при любом ALTER TABLE ADD COLUMN.
	batch, err := b.conn.PrepareBatch(ctx, `INSERT INTO events (
		event_id, project_id, issue_id, timestamp,
		level, message, exception_type, exception_value, stacktrace,
		environment, release, server_name, sdk,
		user_id, user_ip, user_email, tags, contexts,
		trace_id, span_id, breadcrumbs, request)`)
	if err != nil {
		return err
	}
	for _, e := range events {
		id, err := uuid.Parse(e.ID)
		if err != nil {
			id = uuid.New()
		}
		if err := batch.Append(
			id, uint64(e.ProjectID), uint64(e.IssueID), e.Timestamp,
			e.Level, e.Message, e.ExceptionType, e.ExceptionValue, e.Stacktrace,
			e.Environment, e.Release, e.ServerName, e.SDK,
			e.UserID, e.UserIP, e.UserEmail, e.Tags, e.Contexts,
			e.TraceID, e.SpanID, e.Breadcrumbs, e.Request,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// Дефолт рассчитан на инстанс без ограничения памяти — на стеснённом профиле
// (mem_limit 256m) не успевает сработать раньше OOM. 0 и отрицательные — игнорируются.
func (b *Batcher) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.maxBufBytes = n
	b.mu.Unlock()
}
