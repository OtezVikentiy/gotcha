package metric

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHConn — минимум интерфейса ClickHouse, нужный Writer (как trace.CHConn).
type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// metricRow — одна строка metric_points (порядок колонок — как в миграции 0009).
type metricRow struct {
	ProjectID      uint64
	Name           string
	Type           string
	Unit           string
	Service        string
	Environment    string
	Attributes     map[string]string
	TS             time.Time
	Value          float64
	Count          uint64
	BucketCounts   []uint64
	ExplicitBounds []float64
	Monotonic      uint8
	Temporality    string
}

// Writer копит metric-точки и пишет их в ClickHouse пачками (по batchSize или
// тику interval). Тот же паттерн, что trace.SpanWriter: Add никогда не
// блокирует и не возвращает ошибку; неудача вставки возвращает пачку в буфер
// (ретрай); буфер ограничен, при переполнении дропается самое старое.
type Writer struct {
	conn CHConn

	mu          sync.Mutex
	buf         []metricRow
	dropped     int64
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
		maxBuf:    100000,
		batchSize: 1000,
		interval:  5 * time.Second,
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Add кладёт точку в буфер. Никогда не блокирует и не возвращает ошибку: приём
// метрик не должен зависеть от здоровья ClickHouse.
func (w *Writer) Add(projectID int64, p MetricPoint) {
	row := metricRow{
		ProjectID:      uint64(projectID),
		Name:           p.Name,
		Type:           p.Type,
		Unit:           p.Unit,
		Service:        p.Service,
		Environment:    p.Environment,
		Attributes:     p.Attributes,
		TS:             p.TS,
		Value:          p.Value,
		Count:          p.Count,
		BucketCounts:   p.BucketCounts,
		ExplicitBounds: p.ExplicitBounds,
		Temporality:    p.Temporality,
	}
	if p.Monotonic {
		row.Monotonic = 1
	}
	// CH Map/Array не любят nil на Append — приводим к пустым.
	if row.Attributes == nil {
		row.Attributes = map[string]string{}
	}
	if row.BucketCounts == nil {
		row.BucketCounts = []uint64{}
	}
	if row.ExplicitBounds == nil {
		row.ExplicitBounds = []float64{}
	}

	w.mu.Lock()
	logDrop := false
	if drop := len(w.buf) + 1 - w.maxBuf; drop > 0 {
		w.buf = append(w.buf[:0], w.buf[drop:]...)
		w.dropped += int64(drop)
		logDrop = true
	}
	w.buf = append(w.buf, row)
	dropped := w.dropped
	full := len(w.buf) >= w.batchSize
	if logDrop && time.Since(w.lastDropLog) > w.interval {
		w.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	w.mu.Unlock()

	if logDrop {
		slog.Warn("metric buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// Dropped — сколько строк выброшено из-за переполнения буфера.
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
	batch := make([]metricRow, n)
	copy(batch, w.buf[:n])
	w.buf = append(w.buf[:0], w.buf[n:]...)
	w.mu.Unlock()

	if err := w.insert(ctx, batch); err != nil {
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
		slog.Warn("metric batch insert failed, will retry", "rows", len(batch), "error", err, "dropped", over)
	}
}

func (w *Writer) insert(ctx context.Context, rows []metricRow) error {
	batch, err := w.conn.PrepareBatch(ctx, `INSERT INTO metric_points (
		project_id, name, type, unit, service, environment,
		attributes, ts, value, count, bucket_counts, explicit_bounds,
		monotonic, temporality)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.Name, r.Type, r.Unit, r.Service, r.Environment,
			r.Attributes, r.TS, r.Value, r.Count, r.BucketCounts, r.ExplicitBounds,
			r.Monotonic, r.Temporality,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}
