package metric

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/chbatch"
)

type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// Сколько подряд фейлов вставки одного головного батча терпим, прежде чем
// перейти к изоляции ядовитых рядов (chbatch.IsolatePoison).
const poisonThreshold = 3

// Порядок полей соответствует INSERT в insert().
type metricRow struct {
	ProjectID      uint64
	Name           string
	Type           string
	Unit           string
	Service        string
	Environment    string
	Host           string // промоутированный ресурсный host.name
	Attributes     map[string]string
	TS             time.Time
	Value          float64
	Count          uint64
	BucketCounts   []uint64
	ExplicitBounds []float64
	Monotonic      uint8
	Temporality    string
}

// Add никогда не блокирует и не возвращает ошибку; неудачная вставка
// возвращает пачку в буфер (ретрай), переполнение дропает самое старое.
type Writer struct {
	conn CHConn

	mu          sync.Mutex
	buf         []metricRow
	bufBytes    int64 // приблизительный вес buf, см. maxBufBytes
	dropped     int64
	insertFails int64 // накопительно: сколько флашей провалилось
	failStreak  int
	lastDropLog time.Time
	// nil, пока дропов нет — не аллоцируется на горячем пути. Заполняется в
	// trimLocked под mu, сливается в onDrop вне mu (emitDrops).
	pendingDrops map[uint64]int64
	// Сток per-project дропов буфера; nil — no-op (например, в тестах без пайплайна).
	// Читается и пишется под mu.
	onDrop func(projectID uint64, n int64)

	maxBuf      int
	maxBufBytes int64
	batchSize   int
	interval    time.Duration

	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewWriter(conn CHConn) *Writer {
	return &Writer{
		conn:        conn,
		maxBuf:      100000,
		maxBufBytes: defaultMaxBufBytes,
		batchSize:   1000,
		interval:    5 * time.Second,
		kick:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// Приём метрик не должен зависеть от здоровья ClickHouse.
func (w *Writer) Add(projectID int64, p MetricPoint) {
	row := metricRow{
		ProjectID:      uint64(projectID),
		Name:           p.Name,
		Type:           p.Type,
		Unit:           p.Unit,
		Service:        p.Service,
		Environment:    p.Environment,
		Host:           p.Host,
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

	size := metricRowBytes(row)
	w.mu.Lock()
	w.buf = append(w.buf, row)
	w.bufBytes += size
	logDrop := w.trimLocked()
	dropped := w.dropped
	full := len(w.buf) >= w.batchSize
	if logDrop && time.Since(w.lastDropLog) > w.interval {
		w.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	// захватываем под mu, сливаем вне — сток берёт свой мьютекс.
	drops, sink := w.takeDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)

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

// Доп. потолок к потолку по строкам: размер строки задаёт клиент, «сто тысяч
// строк» может оказаться десятками гигабайт.
const defaultMaxBufBytes = 256 << 20

// Постоянная цена строки сверх длины полей — без неё строка из пустых значений
// весила бы почти ноль, и байтовый потолок никогда бы не срабатывал.
const rowOverheadBytes = 64

// 2 заголовка string (16Б каждый) плюс амортизированная цена бакета map —
// без неё Attributes на десятки пар недосчитывался бы кратно.
const attrEntryOverheadBytes = 48

func metricRowBytes(r metricRow) int64 {
	n := len(r.Name) + len(r.Type) + len(r.Unit) + len(r.Service) +
		len(r.Environment) + len(r.Host) + len(r.Temporality) +
		8*len(r.BucketCounts) + 8*len(r.ExplicitBounds)
	for k, v := range r.Attributes {
		n += len(k) + len(v) + attrEntryOverheadBytes
	}
	return int64(n) + rowOverheadBytes
}

// O(числа выброшенных) — вес ведётся инкрементально. Вызывается под mu.
func (w *Writer) trimLocked() bool {
	drop := 0
	if over := len(w.buf) - w.maxBuf; over > 0 {
		drop = over
		for i := 0; i < over; i++ {
			w.bufBytes -= metricRowBytes(w.buf[i])
		}
	}
	// Одну строку оставляем всегда: строка тяжелее потолка сама по себе не повод
	// отдать буфер пустым — она уйдёт ближайшим флашем.
	for drop < len(w.buf)-1 && w.bufBytes > w.maxBufBytes {
		w.bufBytes -= metricRowBytes(w.buf[drop])
		drop++
	}
	if drop <= 0 {
		return false
	}
	// без атрибуции по проекту потеря на буфере писателя невидима пользователю:
	// org_usage.dropped_metrics не растёт, хотя квота уже списана при приёме.
	for i := 0; i < drop; i++ {
		if p := w.buf[i].ProjectID; p > 0 {
			if w.pendingDrops == nil {
				w.pendingDrops = make(map[uint64]int64)
			}
			w.pendingDrops[p]++
		}
	}
	w.buf = append(w.buf[:0], w.buf[drop:]...)
	w.dropped += int64(drop)
	return true
}

// ставится один раз из main до горячего трафика; nil-сток — no-op.
func (w *Writer) SetDropSink(fn func(projectID uint64, n int64)) {
	w.mu.Lock()
	w.onDrop = fn
	w.mu.Unlock()
}

// вызывающий обязан слить результат через reportDrops ПОСЛЕ разблокировки.
func (w *Writer) takeDropsLocked() (map[uint64]int64, func(projectID uint64, n int64)) {
	if len(w.pendingDrops) == 0 {
		return nil, w.onDrop
	}
	m := w.pendingDrops
	w.pendingDrops = nil
	return m, w.onDrop
}

// нужен в flush, где возврат неудачной пачки может переполнить buf уже под mu.
func (w *Writer) emitDrops() {
	w.mu.Lock()
	drops, sink := w.takeDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)
}

func reportDrops(sink func(projectID uint64, n int64), drops map[uint64]int64) {
	if sink == nil {
		return
	}
	for projectID, n := range drops {
		sink(projectID, n)
	}
}

// Нужен там, где буфер перестраивается целиком (возврат пачки после неудачной вставки).
func (w *Writer) recountLocked() {
	w.bufBytes = 0
	for i := range w.buf {
		w.bufBytes += metricRowBytes(w.buf[i])
	}
}

func (w *Writer) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Для самотелеметрии: растущая глубина — первый признак, что хранилище не принимает.
func (w *Writer) Buffered() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.buf))
}

// Отличается от Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется — потеря только при переполнении.
func (w *Writer) InsertFailures() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.insertFails
}

// Максимум по обоим потолкам (строки/байты) — упереться достаточно в один.
// Не обрезается единицей: буфер физически может ненадолго превысить потолок.
func (w *Writer) Saturation() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	rows := bufSaturation(int64(len(w.buf)), int64(w.maxBuf))
	bytes := bufSaturation(w.bufBytes, w.maxBufBytes)
	if bytes > rows {
		return bytes
	}
	return rows
}

// den<=0 — лимит выключен («не ограничены»), не «делить не на что»: не
// паникует и не показывает насыщение.
func bufSaturation(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
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

// Запускать горутиной; завершается через Close.
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

// Доливает остаток буфера при остановке. Идемпотентен.
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
	// возврат неудачной пачки может переполнить buf и вызвать trimLocked —
	// сливаем per-project дропы после того, как секции отпустят mu.
	defer w.emitDrops()
	w.mu.Lock()
	n := min(len(w.buf), w.batchSize)
	if n == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]metricRow, n)
	copy(batch, w.buf[:n])
	w.buf = append(w.buf[:0], w.buf[n:]...)
	w.recountLocked()
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
			w.insertFails++
			// Сбрасываем ТОЛЬКО если изоляция что-то разрешила — иначе при лежащем CH
			// писатель заново запускал бы дробление каждые ~15с впустую.
			if dropped > 0 || len(unresolved) < len(batch) {
				w.failStreak = 0
			}
			var over int
			if len(unresolved) > 0 {
				w.buf = append(unresolved, w.buf...)
				before := w.dropped
				w.recountLocked()
				w.trimLocked()
				over = int(w.dropped - before)
			}
			w.mu.Unlock()
			if dropped > 0 || over > 0 {
				slog.Warn("metric batch: isolated poison rows",
					"dropped", dropped, "unresolved", len(unresolved), "overflow", over, "batch", len(batch))
			}
			return
		}

		w.mu.Lock()
		w.buf = append(batch, w.buf...)
		before := w.dropped
		w.recountLocked()
		w.trimLocked()
		over := int(w.dropped - before)
		w.insertFails++
		w.mu.Unlock()
		slog.Warn("metric batch insert failed, will retry", "rows", len(batch), "error", err, "dropped", over)
		return
	}
	w.mu.Lock()
	w.failStreak = 0
	more := len(w.buf) > 0
	w.mu.Unlock()
	// После всплеска остаток не должен ждать следующего 5с тика: Add() кикает
	// только на переходе через batchSize, повторных киков на том же уровне не шлёт.
	if more {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

func (w *Writer) insert(ctx context.Context, rows []metricRow) error {
	batch, err := w.conn.PrepareBatch(ctx, `INSERT INTO metric_points (
		project_id, name, type, unit, service, environment, host,
		attributes, ts, value, count, bucket_counts, explicit_bounds,
		monotonic, temporality)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.Name, r.Type, r.Unit, r.Service, r.Environment, r.Host,
			r.Attributes, r.TS, r.Value, r.Count, r.BucketCounts, r.ExplicitBounds,
			r.Monotonic, r.Temporality,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

// На стеснённом профиле (mem_limit 256m) 256 МиБ по умолчанию не сработает
// раньше OOM — ставится из main по GOTCHA_MAX_WRITER_BUFFER_BYTES.
func (w *Writer) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	w.maxBufBytes = n
	w.mu.Unlock()
}
