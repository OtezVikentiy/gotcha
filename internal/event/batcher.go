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
	dropped     int64
	failStreak  int
	lastDropLog time.Time

	maxBuf    int
	batchSize int
	interval  time.Duration

	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewBatcher(conn Conn) *Batcher {
	return &Batcher{
		conn:      conn,
		maxBuf:    10000,
		batchSize: 1000,
		interval:  5 * time.Second,
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Add кладёт событие в буфер. Никогда не блокирует и не возвращает ошибку:
// приём событий не должен зависеть от здоровья ClickHouse.
func (b *Batcher) Add(ev Event) {
	b.mu.Lock()
	logDrop := false
	// Bulk-drop: считаем избыток над maxBuf с учётом добавляемого события и
	// сдвигаем разом (O(1) сдвигов на Add вместо O(n) поштучных).
	if drop := len(b.buf) + 1 - b.maxBuf; drop > 0 {
		b.buf = append(b.buf[:0], b.buf[drop:]...)
		b.dropped += int64(drop)
		logDrop = true
	}
	b.buf = append(b.buf, ev)
	dropped := b.dropped
	full := len(b.buf) >= b.batchSize
	if logDrop && time.Since(b.lastDropLog) > b.interval {
		b.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	b.mu.Unlock()
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

// Dropped — сколько событий выброшено из-за переполнения буфера.
func (b *Batcher) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
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

func (b *Batcher) flush(ctx context.Context) {
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
			b.failStreak = 0
			var over int
			if len(unresolved) > 0 {
				b.buf = append(unresolved, b.buf...)
				if over = len(b.buf) - b.maxBuf; over > 0 {
					b.buf = append(b.buf[:0], b.buf[over:]...)
					b.dropped += int64(over)
				} else {
					over = 0
				}
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
		var over int
		if over = len(b.buf) - b.maxBuf; over > 0 {
			b.buf = append(b.buf[:0], b.buf[over:]...)
			b.dropped += int64(over)
		} else {
			over = 0
		}
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
		trace_id, span_id, breadcrumbs)`)
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
			e.TraceID, e.SpanID, e.Breadcrumbs,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}
