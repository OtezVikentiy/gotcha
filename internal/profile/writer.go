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

// после стольких подряд фейлов вставки переходим к изоляции ядовитых рядов дроблением.
const poisonThreshold = 3

type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

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

// unit separator — не встречается в именах функций, кадры не перепутаются при склейке.
const stackSep = "\x1f"

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

// не блокирует на записи в CH и не возвращает ошибку — переполнение буфера тихо
// роняет самые старые строки.
func (w *Writer) Add(projectID int64, p Profile) {
	if len(p.Samples) == 0 {
		return
	}
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

// кап только по строкам не хватает: размер строки задаёт клиент — двести тысяч
// строк могут оказаться десятками гигабайт.
const defaultMaxBufBytes = 256 << 20

// без неё строка из пустых/однобуквенных значений весила бы почти ноль, и
// байтовый потолок не срабатывал бы никогда — только счётный.
const rowOverheadBytes = 64

func profileRowBytes(r profileRow) int64 {
	n := len(r.ProfileType) + len(r.Service) + len(r.Environment) +
		len(r.Transaction) + len(r.Platform) + len(r.Unit) + len(r.TraceID)
	for _, f := range r.Stack {
		// +16 — заголовок string на кадр: без него однобуквенные имена в сотнях
		// кадров недоучитывались бы кратно.
		n += len(f) + 16
	}
	return int64(n) + rowOverheadBytes
}

// O(числа выброшенных): вес ведётся инкрементально в Add. Вызывать под mu.
func (w *Writer) trimLocked() bool {
	drop := 0
	if over := len(w.buf) - w.maxBuf; over > 0 {
		drop = over
		for i := 0; i < over; i++ {
			w.bufBytes -= profileRowBytes(w.buf[i])
		}
	}
	// одна строка остаётся всегда: тяжелее потолка — не повод пустой буфер,
	// уйдёт ближайшим флашем.
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

func (w *Writer) Buffered() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.buf))
}

// не то же самое, что Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется, Dropped растёт только при переполнении буфера.
func (w *Writer) InsertFailures() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.insertFails
}

// худший из двух потолков (строки/байты); значение НЕ обрезано единицей — иначе
// backpressure узнавал бы о переполнении на тик позже, чем оно случилось.
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

// den<=0 значит «лимит выключен», не «нечем делить» — не паниковать и не
// искусственно показывать насыщение.
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
		// data-level «яд» изолируем сразу; транзиент (сеть/ctx) терпим до порога.
		poison := chbatch.IsServerDataError(err)
		w.mu.Lock()
		w.failStreak++
		streak := w.failStreak
		w.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// ядовитые ряды дропнутся, хорошие вставятся, транзиентные вернутся
			// в unresolved без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insert, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.insertFails++
			// сброс только если изоляция что-то разрешила — иначе при лежащем CH
			// писатель заново запускал бы дробление без толку на каждом флаше.
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

// дефолт (defaultMaxBufBytes) рассчитан на инстанс без ограничения памяти: на
// стеснённом профиле (mem_limit 256m) буфер такого размера не успеет сработать раньше OOM.
func (w *Writer) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	w.maxBufBytes = n
	w.mu.Unlock()
}
