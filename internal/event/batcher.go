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

// poisonThreshold — сколько подряд-фейлов вставки одного и того же головного
// батча терпим (транзиентные сбои CH), прежде чем перейти к изоляции ядовитых
// рядов бинарным дроблением (chbatch.IsolatePoison).
const poisonThreshold = 3

// Conn — минимум ClickHouse-интерфейса, нужный батчеру.
type Conn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// Batcher копит события и пишет их в CH пачками: по batchSize или по тику
// interval. Ошибка вставки возвращает пачку в буфер (ретрай следующим
// тиком); буфер ограничен maxBuf, при переполнении дропается самое старое.
type Batcher struct {
	conn Conn

	mu          sync.Mutex
	buf         []Event
	bufBytes    int64 // приблизительный вес buf, см. maxBufBytes
	dropped     int64
	insertFails int64 // накопительно: сколько флашей провалилось
	failStreak  int
	lastDropLog time.Time
	// pendingDrops — выброшенные с прошлого слива строки по orgID, для per-org
	// атрибуции в org_usage.dropped_* (см. onDrop/SetDropSink). nil, пока дропов
	// нет — на горячем пути без потерь не аллоцируется. Заполняется в trimLocked
	// под mu, сливается в onDrop ВНЕ mu (emitDrops).
	pendingDrops map[int64]int64
	// onDrop — сток per-org дропов буфера. Ставится из main (SetDropSink) в
	// pipeline.CountDroppedEvents — ту же in-memory агрегацию, что и дропы
	// очереди, с флашем в org_usage раз в 60с. nil — no-op (учитывать некуда,
	// напр. в тестах писателя без пайплайна). Читается/пишется под mu.
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

// Add кладёт событие в буфер. Никогда не блокирует и не возвращает ошибку:
// приём событий не должен зависеть от здоровья ClickHouse.
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

// SetDropSink задаёт сток per-org дропов буфера (см. Batcher.onDrop). Ставится
// один раз из main до горячего трафика; nil-сток — no-op.
func (b *Batcher) SetDropSink(fn func(orgID, n int64)) {
	b.mu.Lock()
	b.onDrop = fn
	b.mu.Unlock()
}

// takeDropsLocked забирает накопленные per-org дропы и текущий сток. Вызывается
// под mu; вызывающий сливает результат через reportDrops ПОСЛЕ разблокировки.
func (b *Batcher) takeDropsLocked() (map[int64]int64, func(orgID, n int64)) {
	if len(b.pendingDrops) == 0 {
		return nil, b.onDrop
	}
	m := b.pendingDrops
	b.pendingDrops = nil
	return m, b.onDrop
}

// emitDrops сливает накопленные per-org дропы в сток. Для путей, где дроп мог
// случиться под mu, но критическая секция не возвращает сток сама (flush).
func (b *Batcher) emitDrops() {
	b.mu.Lock()
	drops, sink := b.takeDropsLocked()
	b.mu.Unlock()
	reportDrops(sink, drops)
}

// reportDrops вызывает сток по одному разу на организацию. sink==nil или пустая
// карта — no-op.
func reportDrops(sink func(orgID, n int64), drops map[int64]int64) {
	if sink == nil {
		return
	}
	for orgID, n := range drops {
		sink(orgID, n)
	}
}

// Dropped — сколько событий выброшено из-за переполнения буфера.
func (b *Batcher) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Buffered — сколько строк ждёт записи прямо сейчас. Для самотелеметрии:
// растущая глубина буфера — первый признак, что хранилище не принимает.
func (b *Batcher) Buffered() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(len(b.buf))
}

// InsertFailures — сколько флашей провалилось за время жизни процесса.
// Отличается от Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется, потеря наступает только при переполнении буфера.
func (b *Batcher) InsertFailures() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.insertFails
}

// flushWithTimeout ограничивает одну попытку флаша, даже если у parent ctx
// нет собственного дедлайна (context.Background()) или его бюджет большой:
// сетевой чёрный дыр в PrepareBatch/Send не должен вешать Run/Close навсегда.
func (b *Batcher) flushWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	b.flush(ctx)
}

// Run — цикл флаша; запускать горутиной. Завершается через Close.
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

// Close останавливает цикл и доливает остаток буфера. При неудачных
// вставках ретраит с паузой, пока жив ctx; сдаётся только по ctx. Каждая
// попытка флаша ограничена внутренним таймаутом (см. flushWithTimeout), так
// что бюджет ctx остаётся исполнимым даже при зависшей сети. Идемпотентен —
// повторный вызов безопасен и не паникует.
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

// defaultMaxBufBytes — потолок буфера по БАЙТАМ, в дополнение к потолку по
// строкам.
//
// Одного потолка по строкам не хватает: размер строки задаёт клиент. Событие
// несёт четыре сырых JSON-блока (stacktrace/contexts/breadcrumbs/request) по
// maxJSONBlock=256 КиБ каждый, то есть строка доходит до ~1 МиБ, и maxBuf=10000
// таких строк — это больше 10 ГБ в буфере, который заводился под «десять тысяч
// небольших событий». На обычном трафике потолок по строкам срабатывает первым и
// поведение не меняется; байтовый вступает в дело ровно тогда, когда строки
// раздуты.
const defaultMaxBufBytes = 256 << 20

// eventBytes — приблизительный вес события в памяти. Считаем поля, размер
// которых определяет клиент; точность не нужна, нужен порядок величины.
// rowOverheadBytes — постоянная цена ОДНОЙ строки в буфере помимо длины строк:
// заголовки string (16 байт каждый), элемент среза, служебные поля. Без неё
// учёт был обходим тем же приёмом, что и бюджет профилей: строка из пустых или
// однобуквенных значений весила бы почти ноль, и байтовый потолок не срабатывал
// бы никогда — работал бы только счётный.
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

// trimLocked приводит буфер к обоим потолкам, выбрасывая самое старое, и
// поддерживает bufBytes. Стоимость — O(числа выброшенных), а не O(len(buf)):
// вес всего буфера ведётся инкрементально в Add. Вызывается под mu.
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
	// Списываем выброшенные строки их организациям (per-org атрибуция потерь в
	// org_usage.dropped_*): без этого потеря на слое буфера писателя невидима
	// per-org, как была невидима потеря очереди до arch P1-1.
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

// recountLocked пересчитывает вес буфера с нуля. Нужен там, где буфер
// перестраивается целиком (возврат пачки при неудачной вставке), а не растёт
// по одному событию. Вызывается под mu.
func (b *Batcher) recountLocked() {
	b.bufBytes = 0
	for i := range b.buf {
		b.bufBytes += eventBytes(b.buf[i])
	}
}

func (b *Batcher) flush(ctx context.Context) {
	// Возврат провалившейся пачки в буфер тоже может переполнить его и вызвать
	// trimLocked (ниже, в ветках изоляции/ретрая) — сливаем накопленные per-org
	// дропы после того, как критические секции flush отпустят mu.
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
		// Классифицируем ошибку: data-level «яд» изолируем сразу, транзиент
		// (сеть/ctx) терпим до порога и лишь потом эскалируем в изоляцию, где
		// транзиентные ряды вернутся в буфер без потерь.
		poison := chbatch.IsServerDataError(err)
		b.mu.Lock()
		b.failStreak++
		streak := b.failStreak
		b.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved. Дополняет per-value UUID-фолбэк в insert (тот
			// чинит только битый event_id), а не заменяет его.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, b.insert, chbatch.IsServerDataError)
			b.mu.Lock()
			b.dropped += int64(dropped)
			b.insertFails++
			// Сбрасываем счётчик подряд-фейлов ТОЛЬКО если изоляция что-то
			// разрешила. Безусловный сброс означал, что при лежащем
			// ClickHouse писатель заново запускает дробление каждые ~15 с,
			// хотя предыдущая попытка не дала ничего.
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
	// Успех — сбрасываем счётчик подряд-фейлов.
	b.mu.Lock()
	b.failStreak = 0
	b.mu.Unlock()
}

func (b *Batcher) insert(ctx context.Context, events []Event) error {
	// Колонки перечислены явно (в порядке DDL, см. миграции 0001 и 0005):
	// безымянный INSERT требует значение для каждой колонки таблицы и ломается
	// при любом ALTER TABLE ADD COLUMN.
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

// SetMaxBufferBytes задаёт байтовый потолок буфера. Значение по умолчанию
// (defaultMaxBufBytes) рассчитано на инстанс без ограничения памяти; на
// стеснённом профиле (docker-compose.small.yml: mem_limit 256m) буферы по
// 256 МиБ физически не могут сработать раньше OOM-killer'а, то есть защита
// инертна ровно там, где нужнее всего. Ставится из main по
// GOTCHA_MAX_BUFFER_BYTES. Нулевое и отрицательное значение игнорируется.
func (b *Batcher) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.maxBufBytes = n
	b.mu.Unlock()
}
