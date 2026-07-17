package profile

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/chbatch"
)

// poisonThreshold — сколько подряд-фейлов вставки одного и того же головного
// батча терпим (транзиентные сбои CH), прежде чем перейти к изоляции ядовитых
// рядов бинарным дроблением (chbatch.IsolatePoison).
const poisonThreshold = 3

// CHConn — минимум интерфейса ClickHouse, нужный Writer (как metric.CHConn).
type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// profileRow — одна строка profile_samples (уникальный стек профиля).
type profileRow struct {
	ProjectID   uint64
	ProfileType string
	Service     string
	Environment string
	Transaction string
	Platform    string
	TS          time.Time
	Stack       []string
	Value       uint64
	TraceID     string
}

// stackSep — разделитель кадров в ключе схлопывания (unit separator, не
// встречается в именах функций).
const stackSep = "\x1f"

// Writer копит профили и пишет строки profile_samples пачками. Тот же паттерн,
// что metric.Writer: Add неблокирующий, неудача вставки возвращает пачку в
// буфер, буфер ограничен.
type Writer struct {
	conn CHConn

	mu          sync.Mutex
	buf         []profileRow
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

func NewWriter(conn CHConn) *Writer {
	return &Writer{
		conn:      conn,
		maxBuf:    200000,
		batchSize: 1000,
		interval:  5 * time.Second,
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Add раскладывает профиль в строки profile_samples, схлопывая одинаковые стеки
// (сумма value). Никогда не блокирует и не возвращает ошибку.
func (w *Writer) Add(projectID int64, p Profile) {
	if len(p.Samples) == 0 {
		return
	}
	// Схлопывание одинаковых стеков внутри профиля.
	agg := make(map[string]uint64, len(p.Samples))
	keyStacks := make(map[string][]string, len(p.Samples))
	for _, s := range p.Samples {
		keys := make([]string, len(s.Stack))
		for i, f := range s.Stack {
			keys[i] = FrameKey(f)
		}
		k := strings.Join(keys, stackSep)
		agg[k] += s.Value
		if _, ok := keyStacks[k]; !ok {
			keyStacks[k] = keys
		}
	}
	rows := make([]profileRow, 0, len(agg))
	for k, v := range agg {
		rows = append(rows, profileRow{
			ProjectID:   uint64(projectID),
			ProfileType: p.Type,
			Service:     p.Service,
			Environment: p.Environment,
			Transaction: p.Transaction,
			Platform:    p.Platform,
			TS:          p.Timestamp,
			Stack:       keyStacks[k],
			Value:       v,
			TraceID:     p.TraceID,
		})
	}

	w.mu.Lock()
	logDrop := false
	if drop := len(w.buf) + len(rows) - w.maxBuf; drop > 0 {
		if drop > len(w.buf) {
			drop = len(w.buf)
		}
		w.buf = append(w.buf[:0], w.buf[drop:]...)
		w.dropped += int64(drop)
		logDrop = true
	}
	w.buf = append(w.buf, rows...)
	dropped := w.dropped
	full := len(w.buf) >= w.batchSize
	if logDrop && time.Since(w.lastDropLog) > w.interval {
		w.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	w.mu.Unlock()

	if logDrop {
		slog.Warn("profile buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

func (w *Writer) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

func (w *Writer) buffered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.buf)
}

func (w *Writer) flushWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	w.flush(ctx)
}

// Run — цикл флаша; запускать горутиной. Завершается через Close.
func (w *Writer) Run() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
			w.flushWithTimeout(context.Background())
		case <-w.kick:
			w.flushWithTimeout(context.Background())
		}
	}
}

// Close останавливает цикл и доливает остаток буфера. Идемпотентен.
func (w *Writer) Close(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	for {
		n := w.buffered()
		if n == 0 {
			return nil
		}
		w.flushWithTimeout(ctx)
		left := w.buffered()
		if left == 0 {
			return nil
		}
		if left >= n {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func (w *Writer) flush(ctx context.Context) {
	w.mu.Lock()
	n := min(len(w.buf), w.batchSize)
	if n == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]profileRow, n)
	copy(batch, w.buf[:n])
	w.buf = append(w.buf[:0], w.buf[n:]...)
	w.mu.Unlock()

	if err := w.insert(ctx, batch); err != nil {
		// Data-level «яд» изолируем сразу; транзиент (сеть/ctx) терпим до порога.
		poison := chbatch.IsServerDataError(err)
		w.mu.Lock()
		w.failStreak++
		streak := w.failStreak
		w.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved (обратно в буфер) без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insert, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.failStreak = 0
			var over int
			if len(unresolved) > 0 {
				w.buf = append(unresolved, w.buf...)
				if over = len(w.buf) - w.maxBuf; over > 0 {
					w.buf = append(w.buf[:0], w.buf[over:]...)
					w.dropped += int64(over)
				} else {
					over = 0
				}
			}
			w.mu.Unlock()
			if dropped > 0 || over > 0 {
				slog.Warn("profile batch: isolated poison rows",
					"dropped", dropped, "unresolved", len(unresolved), "overflow", over, "batch", len(batch))
			}
			return
		}

		w.mu.Lock()
		w.buf = append(batch, w.buf...)
		over := len(w.buf) - w.maxBuf
		if over > 0 {
			w.buf = append(w.buf[:0], w.buf[over:]...)
			w.dropped += int64(over)
		} else {
			over = 0
		}
		w.mu.Unlock()
		slog.Warn("profile batch insert failed, will retry", "rows", len(batch), "error", err, "dropped", over)
		return
	}
	// Успех — сбрасываем счётчик подряд-фейлов.
	w.mu.Lock()
	w.failStreak = 0
	w.mu.Unlock()
}

func (w *Writer) insert(ctx context.Context, rows []profileRow) error {
	batch, err := w.conn.PrepareBatch(ctx, `INSERT INTO profile_samples (
		project_id, profile_type, service, environment, transaction, platform, ts, stack, value, trace_id)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.ProfileType, r.Service, r.Environment, r.Transaction, r.Platform, r.TS, r.Stack, r.Value, r.TraceID,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}
