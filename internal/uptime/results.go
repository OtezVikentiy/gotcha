package uptime

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// CHConn — минимум интерфейса ClickHouse, нужный ResultWriter.
type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// resultRow — одна строка на запись в CH-таблицу check_results.
type resultRow struct {
	ProjectID int64
	MonitorID int64
	Region    string
	At        time.Time
	Result    Result
}

// ResultWriter копит результаты проверок и пишет их в ClickHouse пачками:
// по batchSize или по тику interval. Повторяет паттерн event.Batcher (см.
// internal/event/batcher.go): Add никогда не блокирует и не возвращает
// ошибку, ошибка вставки возвращает пачку в буфер (ретрай следующим тиком),
// буфер ограничен maxBuf, при переполнении дропается самое старое.
type ResultWriter struct {
	conn CHConn

	mu          sync.Mutex
	buf         []resultRow
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

func NewResultWriter(conn CHConn) *ResultWriter {
	return &ResultWriter{
		conn:      conn,
		maxBuf:    10000,
		batchSize: 1000,
		interval:  5 * time.Second,
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Add кладёт результат проверки в буфер. Никогда не блокирует и не
// возвращает ошибку: приём результатов не должен зависеть от здоровья
// ClickHouse.
func (w *ResultWriter) Add(projectID, monitorID int64, region string, at time.Time, r Result) {
	w.mu.Lock()
	logDrop := false
	if len(w.buf) >= w.maxBuf {
		w.buf = append(w.buf[:0], w.buf[1:]...)
		w.dropped++
		if time.Since(w.lastDropLog) > w.interval {
			w.lastDropLog = time.Now()
			logDrop = true
		}
	}
	dropped := w.dropped
	w.buf = append(w.buf, resultRow{ProjectID: projectID, MonitorID: monitorID, Region: region, At: at, Result: r})
	full := len(w.buf) >= w.batchSize
	w.mu.Unlock()
	if logDrop {
		slog.Warn("check result buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// Dropped — сколько результатов выброшено из-за переполнения буфера.
func (w *ResultWriter) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// flushWithTimeout ограничивает одну попытку флаша, даже если у parent ctx
// нет собственного дедлайна (context.Background()) или его бюджет большой:
// сетевой чёрный дыр в PrepareBatch/Send не должен вешать Run/Close навсегда.
func (w *ResultWriter) flushWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	w.flush(ctx)
}

// Run — цикл флаша; запускать горутиной. Завершается через Close.
func (w *ResultWriter) Run() {
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

// Close останавливает цикл и доливает остаток буфера. При неудачных
// вставках ретраит с паузой, пока жив ctx; сдаётся только по ctx. Каждая
// попытка флаша ограничена внутренним таймаутом (см. flushWithTimeout), так
// что бюджет ctx остаётся исполнимым даже при зависшей сети. Идемпотентен —
// повторный вызов безопасен и не паникует.
func (w *ResultWriter) Close(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	err := w.closeDrain(ctx)
	if dropped := w.Dropped(); dropped > 0 {
		slog.Warn("check results dropped during lifetime", "dropped_total", dropped)
	}
	return err
}

func (w *ResultWriter) closeDrain(ctx context.Context) error {
	for {
		w.mu.Lock()
		n := len(w.buf)
		w.mu.Unlock()
		if n == 0 {
			return nil
		}
		w.flushWithTimeout(ctx)
		w.mu.Lock()
		left := len(w.buf)
		w.mu.Unlock()
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

func (w *ResultWriter) flush(ctx context.Context) {
	w.mu.Lock()
	if len(w.buf) == 0 {
		w.mu.Unlock()
		return
	}
	n := len(w.buf)
	if n > w.batchSize {
		n = w.batchSize
	}
	batch := make([]resultRow, n)
	copy(batch, w.buf[:n])
	w.buf = append(w.buf[:0], w.buf[n:]...)
	w.mu.Unlock()

	if err := w.insert(ctx, batch); err != nil {
		w.mu.Lock()
		w.buf = append(batch, w.buf...)
		var over int
		if over = len(w.buf) - w.maxBuf; over > 0 {
			w.buf = append(w.buf[:0], w.buf[over:]...)
			w.dropped += int64(over)
		} else {
			over = 0
		}
		w.mu.Unlock()
		slog.Warn("check result batch insert failed, will retry",
			"rows", len(batch), "error", err, "dropped", over)
	}
}

func boolToUint8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func (w *ResultWriter) insert(ctx context.Context, rows []resultRow) error {
	// ВНИМАНИЕ: INSERT без списка колонок требует значение для КАЖДОЙ колонки
	// check_results в порядке объявления. Добавляете колонку в миграции —
	// обязаны поправить и этот Append (или перейти на явный список колонок,
	// как сделано в internal/event/batcher.go), иначе вставка результатов
	// проверок сломается в рантайме.
	batch, err := w.conn.PrepareBatch(ctx, "INSERT INTO check_results")
	if err != nil {
		return err
	}
	for _, row := range rows {
		r := row.Result
		if err := batch.Append(
			uint64(row.MonitorID), uint64(row.ProjectID), row.Region, row.At,
			boolToUint8(r.OK), uint16(r.StatusCode), r.Error,
			r.DNSMs, r.ConnectMs, r.TLSMs, r.TTFBMs, r.TotalMs, r.BodySize,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}
