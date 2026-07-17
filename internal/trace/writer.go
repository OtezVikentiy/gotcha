package trace

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"gitflic.ru/otezvikentiy/gotcha/internal/chbatch"
)

// poisonThreshold — сколько подряд-фейлов вставки одного и того же головного
// батча терпим (транзиентные сбои CH), прежде чем перейти к изоляции ядовитых
// рядов бинарным дроблением (chbatch.IsolatePoison).
const poisonThreshold = 3

// CHConn — минимум интерфейса ClickHouse, нужный SpanWriter.
type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// txRow — одна строка CH-таблицы transactions (порядок колонок — как в
// миграции 0003_traces).
type txRow struct {
	ProjectID   uint64
	TraceID     string
	SpanID      string
	Transaction string
	Op          string
	Timestamp   time.Time
	DurationUS  uint32
	Status      string
	Environment string
	Release     string
	ServerName  string
	UserID      string
	Tags        map[string]string
	Source      string
	// Measurements уезжает в CH-колонку measurements Map(String, Float64); nil
	// приводится к пустому map при заполнении строки (CH Map не любит nil).
	Measurements map[string]float64
}

// spanRow — одна строка CH-таблицы spans.
type spanRow struct {
	ProjectID       uint64
	TraceID         string
	SpanID          string
	ParentSpanID    string
	Transaction     string
	Op              string
	Description     string
	DescriptionHash uint64
	Timestamp       time.Time
	DurationUS      uint32
	Status          string
	Environment     string
	Data            string
	Source          string
}

// SpanWriter копит транзакции и пишет их в ClickHouse пачками: по batchSize
// или по тику interval. Повторяет паттерн event.Batcher / uptime.ResultWriter
// (см. internal/event/batcher.go): Add никогда не блокирует и не возвращает
// ошибку, ошибка вставки возвращает пачку в буфер (ретрай следующим тиком),
// буфер ограничен, при переполнении дропается самое старое.
//
// Отличие от предшественников — две таблицы (transactions и spans) и потому
// два независимых буфера: неудача вставки в одну таблицу не заставляет
// переотправлять уже вставленные строки другой (иначе были бы дубли).
type SpanWriter struct {
	conn CHConn

	mu          sync.Mutex
	txBuf       []txRow
	spanBuf     []spanRow
	dropped     int64
	lastDropLog time.Time
	// Два независимых батча (transactions и spans) → два раздельных счётчика
	// подряд-фейлов: изоляция ядовитых рядов включается по каждой таблице отдельно.
	txFailStreak   int
	spanFailStreak int

	maxBuf        int
	maxSpanBuf    int
	batchSize     int
	spanBatchSize int
	interval      time.Duration

	kick     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewSpanWriter(conn CHConn) *SpanWriter {
	return &SpanWriter{
		conn: conn,
		// Спанов на порядок больше, чем транзакций, — и буфер, и пачка шире.
		maxBuf:        10000,
		maxSpanBuf:    100000,
		batchSize:     1000,
		spanBatchSize: 10000,
		interval:      5 * time.Second,
		kick:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// Add кладёт транзакцию в буферы: 1 строка в transactions и len(Spans)+1
// строк в spans (корневой спан тоже попадает в spans). Никогда не блокирует и
// не возвращает ошибку: приём транзакций не должен зависеть от здоровья
// ClickHouse.
func (w *SpanWriter) Add(projectID int64, t Transaction) {
	tx := txRow{
		ProjectID:    uint64(projectID),
		TraceID:      t.TraceID,
		SpanID:       t.SpanID,
		Transaction:  t.Name,
		Op:           t.Op,
		Timestamp:    t.Start,
		DurationUS:   t.DurationUS(),
		Status:       t.Status,
		Environment:  t.Environment,
		Release:      t.Release,
		ServerName:   t.ServerName,
		UserID:       t.UserID,
		Tags:         t.Tags,
		Source:       t.Source,
		Measurements: t.Measurements,
	}
	if tx.Tags == nil {
		tx.Tags = map[string]string{}
	}
	// CH Map не принимает nil-карту на Append — как и tags, приводим к пустой.
	if tx.Measurements == nil {
		tx.Measurements = map[string]float64{}
	}

	spans := make([]spanRow, 0, len(t.Spans)+1)
	// Корневой спан: без родителя, описание — имя транзакции.
	spans = append(spans, spanRow{
		ProjectID:       uint64(projectID),
		TraceID:         t.TraceID,
		SpanID:          t.SpanID,
		Transaction:     t.Name,
		Op:              t.Op,
		Description:     t.Name,
		DescriptionHash: DescriptionHash(t.Op, t.Name),
		Timestamp:       t.Start,
		DurationUS:      t.DurationUS(),
		Status:          t.Status,
		Environment:     t.Environment,
		Data:            "{}",
		Source:          t.Source,
	})
	for _, s := range t.Spans {
		spans = append(spans, spanRow{
			ProjectID:       uint64(projectID),
			TraceID:         t.TraceID,
			SpanID:          s.SpanID,
			ParentSpanID:    s.ParentSpanID,
			Transaction:     t.Name, // спаны наследуют имя/окружение транзакции
			Op:              s.Op,
			Description:     s.Description,
			DescriptionHash: DescriptionHash(s.Op, s.Description),
			Timestamp:       s.Start,
			DurationUS:      s.DurationUS(),
			Status:          s.Status,
			Environment:     t.Environment,
			Data:            encodeData(s.Data),
			Source:          t.Source,
		})
	}

	w.mu.Lock()
	logDrop := false
	if drop := len(w.txBuf) + 1 - w.maxBuf; drop > 0 {
		w.txBuf = append(w.txBuf[:0], w.txBuf[drop:]...)
		w.dropped += int64(drop)
		logDrop = true
	}
	if drop := len(w.spanBuf) + len(spans) - w.maxSpanBuf; drop > 0 {
		if drop > len(w.spanBuf) {
			drop = len(w.spanBuf)
		}
		w.spanBuf = append(w.spanBuf[:0], w.spanBuf[drop:]...)
		w.dropped += int64(drop)
		logDrop = true
	}
	w.txBuf = append(w.txBuf, tx)
	w.spanBuf = append(w.spanBuf, spans...)
	dropped := w.dropped
	full := len(w.txBuf) >= w.batchSize || len(w.spanBuf) >= w.spanBatchSize
	if logDrop && time.Since(w.lastDropLog) > w.interval {
		w.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	w.mu.Unlock()

	if logDrop {
		slog.Warn("trace buffer full, dropping oldest", "dropped_total", dropped)
	}
	if full {
		select {
		case w.kick <- struct{}{}:
		default:
		}
	}
}

// encodeData сериализует data спана в JSON; пустая карта и несериализуемое
// значение дают "{}" — колонка data всегда валидный JSON.
func encodeData(data map[string]any) string {
	if len(data) == 0 {
		return "{}"
	}
	b, err := json.Marshal(data)
	if err != nil {
		slog.Warn("span data is not serializable, storing empty object", "error", err)
		return "{}"
	}
	return string(b)
}

// Dropped — сколько строк (транзакций и спанов) выброшено из-за переполнения
// буферов.
func (w *SpanWriter) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// flushWithTimeout ограничивает одну попытку флаша, даже если у parent ctx
// нет собственного дедлайна (context.Background()) или его бюджет большой:
// сетевая чёрная дыра в PrepareBatch/Send не должна вешать Run/Close навсегда.
func (w *SpanWriter) flushWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	w.flush(ctx)
}

// Run — цикл флаша; запускать горутиной. Завершается через Close.
func (w *SpanWriter) Run() {
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

// Close останавливает цикл и доливает остаток буферов. При неудачных вставках
// ретраит с паузой, пока жив ctx; сдаётся только по ctx. Каждая попытка флаша
// ограничена внутренним таймаутом (см. flushWithTimeout), так что бюджет ctx
// остаётся исполнимым даже при зависшей сети. Идемпотентен — повторный вызов
// безопасен и не паникует.
func (w *SpanWriter) Close(ctx context.Context) error {
	w.stopOnce.Do(func() { close(w.stop) })
	<-w.done
	err := w.closeDrain(ctx)
	if dropped := w.Dropped(); dropped > 0 {
		slog.Warn("trace rows dropped during lifetime", "dropped_total", dropped)
	}
	return err
}

func (w *SpanWriter) closeDrain(ctx context.Context) error {
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
		if left >= n { // флаш не продвинулся — пауза перед ретраем
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func (w *SpanWriter) buffered() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.txBuf) + len(w.spanBuf)
}

// flush пишет по одной пачке в каждую таблицу. Таблицы независимы: неудача
// одной не откатывает другую.
func (w *SpanWriter) flush(ctx context.Context) {
	w.flushTx(ctx)
	w.flushSpans(ctx)
}

func (w *SpanWriter) flushTx(ctx context.Context) {
	w.mu.Lock()
	n := min(len(w.txBuf), w.batchSize)
	if n == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]txRow, n)
	copy(batch, w.txBuf[:n])
	w.txBuf = append(w.txBuf[:0], w.txBuf[n:]...)
	w.mu.Unlock()

	if err := w.insertTx(ctx, batch); err != nil {
		// Data-level «яд» изолируем сразу; транзиент (сеть/ctx) терпим до порога.
		poison := chbatch.IsServerDataError(err)
		w.mu.Lock()
		w.txFailStreak++
		streak := w.txFailStreak
		w.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved (обратно в буфер) без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insertTx, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.txFailStreak = 0
			var over int
			if len(unresolved) > 0 {
				w.txBuf = append(unresolved, w.txBuf...)
				if over = len(w.txBuf) - w.maxBuf; over > 0 {
					w.txBuf = append(w.txBuf[:0], w.txBuf[over:]...)
					w.dropped += int64(over)
				} else {
					over = 0
				}
			}
			w.mu.Unlock()
			if dropped > 0 || over > 0 {
				slog.Warn("transaction batch: isolated poison rows",
					"dropped", dropped, "unresolved", len(unresolved), "overflow", over, "batch", len(batch))
			}
			return
		}

		w.mu.Lock()
		w.txBuf = append(batch, w.txBuf...)
		over := len(w.txBuf) - w.maxBuf
		if over > 0 {
			w.txBuf = append(w.txBuf[:0], w.txBuf[over:]...)
			w.dropped += int64(over)
		} else {
			over = 0
		}
		w.mu.Unlock()
		slog.Warn("transaction batch insert failed, will retry",
			"rows", len(batch), "error", err, "dropped", over)
		return
	}
	// Успех — сбрасываем счётчик подряд-фейлов tx.
	w.mu.Lock()
	w.txFailStreak = 0
	w.mu.Unlock()
}

func (w *SpanWriter) flushSpans(ctx context.Context) {
	w.mu.Lock()
	n := min(len(w.spanBuf), w.spanBatchSize)
	if n == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]spanRow, n)
	copy(batch, w.spanBuf[:n])
	w.spanBuf = append(w.spanBuf[:0], w.spanBuf[n:]...)
	w.mu.Unlock()

	if err := w.insertSpans(ctx, batch); err != nil {
		// Data-level «яд» изолируем сразу; транзиент (сеть/ctx) терпим до порога.
		poison := chbatch.IsServerDataError(err)
		w.mu.Lock()
		w.spanFailStreak++
		streak := w.spanFailStreak
		w.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved (обратно в буфер) без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insertSpans, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.spanFailStreak = 0
			var over int
			if len(unresolved) > 0 {
				w.spanBuf = append(unresolved, w.spanBuf...)
				if over = len(w.spanBuf) - w.maxSpanBuf; over > 0 {
					w.spanBuf = append(w.spanBuf[:0], w.spanBuf[over:]...)
					w.dropped += int64(over)
				} else {
					over = 0
				}
			}
			w.mu.Unlock()
			if dropped > 0 || over > 0 {
				slog.Warn("span batch: isolated poison rows",
					"dropped", dropped, "unresolved", len(unresolved), "overflow", over, "batch", len(batch))
			}
			return
		}

		w.mu.Lock()
		w.spanBuf = append(batch, w.spanBuf...)
		over := len(w.spanBuf) - w.maxSpanBuf
		if over > 0 {
			w.spanBuf = append(w.spanBuf[:0], w.spanBuf[over:]...)
			w.dropped += int64(over)
		} else {
			over = 0
		}
		w.mu.Unlock()
		slog.Warn("span batch insert failed, will retry",
			"rows", len(batch), "error", err, "dropped", over)
		return
	}
	// Успех — сбрасываем счётчик подряд-фейлов spans.
	w.mu.Lock()
	w.spanFailStreak = 0
	w.mu.Unlock()
}

func (w *SpanWriter) insertTx(ctx context.Context, rows []txRow) error {
	batch, err := w.conn.PrepareBatch(ctx, `INSERT INTO transactions (
		project_id, trace_id, span_id, transaction, op,
		timestamp, duration_us, status, environment,
		release, server_name, user_id, tags, source, measurements)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.TraceID, r.SpanID, r.Transaction, r.Op,
			r.Timestamp, r.DurationUS, r.Status, r.Environment,
			r.Release, r.ServerName, r.UserID, r.Tags, r.Source, r.Measurements,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (w *SpanWriter) insertSpans(ctx context.Context, rows []spanRow) error {
	batch, err := w.conn.PrepareBatch(ctx, `INSERT INTO spans (
		project_id, trace_id, span_id, parent_span_id, transaction, op,
		description, description_hash, timestamp, duration_us,
		status, environment, data, source)`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := batch.Append(
			r.ProjectID, r.TraceID, r.SpanID, r.ParentSpanID, r.Transaction, r.Op,
			r.Description, r.DescriptionHash, r.Timestamp, r.DurationUS,
			r.Status, r.Environment, r.Data, r.Source,
		); err != nil {
			return err
		}
	}
	return batch.Send()
}
