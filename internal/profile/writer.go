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
	Unit        string
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
		maxBuf:      200000,
		maxBufBytes: defaultMaxBufBytes,
		batchSize:   1000,
		interval:    5 * time.Second,
		kick:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
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
			Unit:        p.Unit,
			TraceID:     p.TraceID,
		})
	}

	size := int64(0)
	for i := range rows {
		size += profileRowBytes(rows[i])
	}
	w.mu.Lock()
	w.buf = append(w.buf, rows...)
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
		slog.Warn("profile buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// defaultMaxBufBytes — потолок буфера по БАЙТАМ, в дополнение к потолку по
// строкам. Одного потолка по строкам не хватает: размер строки задаёт клиент,
// поэтому «двести тысяч строк» могут оказаться десятками гигабайт. На обычном
// трафике первым срабатывает потолок по строкам и поведение не меняется.
const defaultMaxBufBytes = 256 << 20

// rowOverheadBytes — постоянная цена ОДНОЙ строки в буфере помимо длины строк:
// заголовки string (16 байт каждый), элемент среза, служебные поля. Без неё
// учёт был обходим тем же приёмом, что и бюджет профилей: строка из пустых или
// однобуквенных значений весила бы почти ноль, и байтовый потолок не срабатывал
// бы никогда — работал бы только счётный.
const rowOverheadBytes = 64

func profileRowBytes(r profileRow) int64 {
	n := len(r.ProfileType) + len(r.Service) + len(r.Environment) +
		len(r.Transaction) + len(r.Platform) + len(r.Unit) + len(r.TraceID)
	for _, f := range r.Stack {
		// +16 — заголовок string на кадр: у профиля Stack может быть в сотни
		// кадров при однобуквенных именах, и без этого недоучёт достигал 16×.
		n += len(f) + 16
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
			w.bufBytes -= profileRowBytes(w.buf[i])
		}
	}
	// Одну строку оставляем всегда: строка тяжелее потолка сама по себе не повод
	// отдать буфер пустым — она уйдёт ближайшим флашем.
	for drop < len(w.buf)-1 && w.bufBytes > w.maxBufBytes {
		w.bufBytes -= profileRowBytes(w.buf[drop])
		drop++
	}
	if drop <= 0 {
		return false
	}
	w.buf = append(w.buf[:0], w.buf[drop:]...)
	w.dropped += int64(drop)
	return true
}

// recountLocked пересчитывает вес с нуля — нужен там, где буфер перестраивается
// целиком (возврат пачки после неудачной вставки).
func (w *Writer) recountLocked() {
	w.bufBytes = 0
	for i := range w.buf {
		w.bufBytes += profileRowBytes(w.buf[i])
	}
}

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
	batch := make([]profileRow, n)
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
				slog.Warn("profile batch: isolated poison rows",
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
		project_id, profile_type, service, environment, transaction, platform, ts, stack, value, unit, trace_id)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.ProfileType, r.Service, r.Environment, r.Transaction, r.Platform, r.TS, r.Stack, r.Value, r.Unit, r.TraceID,
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
func (w *Writer) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	w.maxBufBytes = n
	w.mu.Unlock()
}
