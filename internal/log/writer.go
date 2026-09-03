package log

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/chbatch"
)

// CHConn — минимум интерфейса ClickHouse, нужный Writer (как metric.CHConn).
type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// poisonThreshold — сколько подряд-фейлов вставки одного и того же головного
// батча терпим (транзиентные сбои CH), прежде чем перейти к изоляции ядовитых
// рядов бинарным дроблением (chbatch.IsolatePoison).
const poisonThreshold = 3

// logRow — одна строка logs (порядок колонок соответствует INSERT в insert()).
type logRow struct {
	ProjectID      uint64
	Timestamp      time.Time
	ObservedTS     time.Time
	Severity       string
	SeverityNumber uint8
	SeverityText   string
	Body           string
	TraceID        string
	SpanID         string
	LogAttributes  map[string]string
	ResourceAttrs  map[string]string
	Service        string
	Environment    string
}

// Writer копит записи логов и пишет их в ClickHouse пачками (по batchSize
// или тику interval). Тот же паттерн, что metric.Writer: Add никогда не
// блокирует и не возвращает ошибку; неудача вставки возвращает пачку в буфер
// (ретрай); буфер ограничен, при переполнении дропается самое старое.
type Writer struct {
	conn CHConn

	mu          sync.Mutex
	buf         []logRow
	bufBytes    int64 // приблизительный вес buf, см. maxBufBytes
	dropped     int64
	insertFails int64 // накопительно: сколько флашей провалилось
	failStreak  int
	lastDropLog time.Time

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

// Add кладёт запись в буфер. Никогда не блокирует и не возвращает ошибку:
// приём логов не должен зависеть от здоровья ClickHouse.
func (w *Writer) Add(projectID int64, r LogRecord) {
	row := logRow{
		ProjectID:      uint64(projectID),
		Timestamp:      r.Timestamp,
		ObservedTS:     r.ObservedTS,
		Severity:       r.Severity,
		SeverityNumber: r.SeverityNumber,
		SeverityText:   r.SeverityText,
		Body:           r.Body,
		TraceID:        r.TraceID,
		SpanID:         r.SpanID,
		LogAttributes:  r.LogAttributes,
		ResourceAttrs:  r.ResourceAttrs,
		Service:        r.Service,
		Environment:    r.Environment,
	}
	// CH Map не любит nil на Append — приводим к пустой карте.
	if row.LogAttributes == nil {
		row.LogAttributes = map[string]string{}
	}
	if row.ResourceAttrs == nil {
		row.ResourceAttrs = map[string]string{}
	}

	size := logRowBytes(row)
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
	w.mu.Unlock()

	if logDrop {
		slog.Warn("log buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// defaultMaxBufBytes — потолок буфера по БАЙТАМ, в дополнение к потолку по
// строкам. Одного потолка по строкам не хватает: размер строки задаёт клиент
// (body лога — до 64 КиБ), поэтому «сто тысяч строк» могут оказаться
// десятками гигабайт. На обычном трафике первым срабатывает потолок по
// строкам и поведение не меняется.
const defaultMaxBufBytes = 256 << 20

// rowOverheadBytes — постоянная цена ОДНОЙ строки в буфере помимо длины строк:
// заголовки string (16 байт каждый), элемент среза, служебные поля. Без неё
// учёт был обходим тем же приёмом, что и бюджет профилей: строка из пустых
// или однобуквенных значений весила бы почти ноль, и байтовый потолок не
// срабатывал бы никогда — работал бы только счётный.
const rowOverheadBytes = 64

// logRowBytes считает вес строки ПОИМЕННО, обязательно включая body: тело
// лога до 64 КиБ на строку — если его не учесть, буфер при maxBufBytes
// дорастёт до гигабайт, а trimLocked никогда не сработает.
func logRowBytes(r logRow) int64 {
	n := len(r.Body) + len(r.Severity) + len(r.SeverityText) +
		len(r.TraceID) + len(r.SpanID) + len(r.Service) + len(r.Environment)
	for k, v := range r.LogAttributes {
		n += len(k) + len(v)
	}
	for k, v := range r.ResourceAttrs {
		n += len(k) + len(v)
	}
	return int64(n) + rowOverheadBytes
}

// trimLocked приводит буфер к обоим потолкам, выбрасывая самое старое.
// Стоимость — O(числа выброшенных): вес ведётся инкрементально в Add.
// Вызывается под mu.
func (w *Writer) trimLocked() bool {
	drop := 0
	if over := len(w.buf) - w.maxBuf; over > 0 {
		drop = over
		for i := 0; i < over; i++ {
			w.bufBytes -= logRowBytes(w.buf[i])
		}
	}
	// Одну строку оставляем всегда: строка тяжелее потолка сама по себе не повод
	// отдать буфер пустым — она уйдёт ближайшим флашем.
	for drop < len(w.buf)-1 && w.bufBytes > w.maxBufBytes {
		w.bufBytes -= logRowBytes(w.buf[drop])
		drop++
	}
	if drop <= 0 {
		return false
	}
	w.buf = append(w.buf[:0], w.buf[drop:]...)
	w.dropped += int64(drop)
	return true
}

// recountLocked пересчитывает вес с нуля — нужен там, где буфер
// перестраивается целиком (возврат пачки после неудачной вставки).
func (w *Writer) recountLocked() {
	w.bufBytes = 0
	for i := range w.buf {
		w.bufBytes += logRowBytes(w.buf[i])
	}
}

// Dropped — сколько строк выброшено из-за переполнения буфера.
func (w *Writer) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Buffered — сколько строк ждёт записи прямо сейчас. Для самотелеметрии:
// растущая глубина буфера — первый признак, что хранилище не принимает.
func (w *Writer) Buffered() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.buf))
}

// InsertFailures — сколько флашей провалилось за время жизни процесса.
// Отличается от Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется, потеря наступает только при переполнении буфера.
func (w *Writer) InsertFailures() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.insertFails
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
	batch := make([]logRow, n)
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
			// Сбрасываем счётчик подряд-фейлов ТОЛЬКО если изоляция что-то
			// разрешила. Безусловный сброс означал, что при лежащем
			// ClickHouse писатель заново запускает дробление каждые ~15 с,
			// хотя предыдущая попытка не дала ничего.
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
				slog.Warn("log batch: isolated poison rows",
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
		slog.Warn("log batch insert failed, will retry", "rows", len(batch), "error", err, "dropped", over)
		return
	}
	// Успех — сбрасываем счётчик подряд-фейлов.
	w.mu.Lock()
	w.failStreak = 0
	w.mu.Unlock()
}

func (w *Writer) insert(ctx context.Context, rows []logRow) error {
	batch, err := w.conn.PrepareBatch(ctx, `INSERT INTO logs (
		project_id, timestamp, observed_ts, severity, severity_number, severity_text,
		body, trace_id, span_id, log_attributes, resource_attrs, service, environment)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.Timestamp, r.ObservedTS, r.Severity, r.SeverityNumber, r.SeverityText,
			r.Body, r.TraceID, r.SpanID, r.LogAttributes, r.ResourceAttrs, r.Service, r.Environment,
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
// GOTCHA_MAX_WRITER_BUFFER_BYTES. Нулевое и отрицательное значение игнорируется.
func (w *Writer) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	w.maxBufBytes = n
	w.mu.Unlock()
}
