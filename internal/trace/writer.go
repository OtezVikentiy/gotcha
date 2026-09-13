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

// сколько подряд-фейлов транзиентных сбоев терпим, прежде чем перейти к
// изоляции ядовитых рядов бинарным дроблением (chbatch.IsolatePoison).
const poisonThreshold = 3

type CHConn interface {
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// порядок полей должен совпадать со списком колонок в insertTx.
type txRow struct {
	// не пишется в CH — только для per-org атрибуции дропов (см. SetDropSink);
	// 0 — атрибутировать некуда.
	OrgID       int64
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
	// уезжает в CH-колонку measurements Map(String, Float64); nil приводится к
	// пустой map — CH Map не любит nil.
	Measurements map[string]float64
}

type spanRow struct {
	// не пишется в CH — только для per-org атрибуции дропов spanBuf (см.
	// SetSpanDropSink); 0 — атрибутировать некуда.
	OrgID           int64
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

// два независимых буфера (transactions/spans) — неудача вставки в одну
// таблицу не переотправляет уже вставленные строки другой (иначе дубли).
type SpanWriter struct {
	conn CHConn

	mu          sync.Mutex
	txBuf       []txRow
	txBytes     int64 // приблизительный вес txBuf, см. maxBufBytes
	spanBuf     []spanRow
	spanBytes   int64 // приблизительный вес spanBuf
	dropped     int64
	insertFails int64 // накопительно: сколько флашей провалилось
	lastDropLog time.Time
	// два раздельных счётчика — изоляция ядовитых рядов включается по каждой
	// таблице отдельно.
	txFailStreak   int
	spanFailStreak int
	// только txBuf — транзакция это квота/биллинг-единица, дроп спана из
	// spanBuf в счётчик не идёт; заполняется под mu, сливается вне (emitDrops).
	pendingDrops map[int64]int64
	onDrop       func(orgID, n int64) // сток per-org дропов txBuf; nil — no-op. Под mu.
	// дропы spanBuf: не квота (билится только транзакция), отдельный сток —
	// чтобы не задваивать/не путать с dropped_transactions. Под mu.
	pendingSpanDrops map[int64]int64
	onSpanDrop       func(orgID, n int64)

	maxBuf        int
	maxSpanBuf    int
	maxBufBytes   int64
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
		maxBufBytes:   defaultMaxBufBytes,
		batchSize:     1000,
		spanBatchSize: 10000,
		interval:      5 * time.Second,
		kick:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
	}
}

// никогда не блокирует и не возвращает ошибку — приём не должен зависеть от
// здоровья ClickHouse.
func (w *SpanWriter) Add(orgID, projectID int64, t Transaction) {
	tx := txRow{
		OrgID:        orgID,
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
	spans = append(spans, spanRow{
		OrgID:           orgID,
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
			OrgID:           orgID,
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

	txSize := txRowBytes(tx)
	spanSize := int64(0)
	for i := range spans {
		spanSize += spanRowBytes(spans[i])
	}

	w.mu.Lock()
	w.txBuf = append(w.txBuf, tx)
	w.txBytes += txSize
	w.spanBuf = append(w.spanBuf, spans...)
	w.spanBytes += spanSize
	logDrop := w.trimTxLocked()
	if w.trimSpansLocked() {
		logDrop = true
	}
	dropped := w.dropped
	full := len(w.txBuf) >= w.batchSize || len(w.spanBuf) >= w.spanBatchSize
	if logDrop && time.Since(w.lastDropLog) > w.interval {
		w.lastDropLog = time.Now()
	} else {
		logDrop = false
	}
	// захватываем под mu, сливаем вне — сток берёт свой мьютекс.
	drops, sink := w.takeDropsLocked()
	spanDrops, spanSink := w.takeSpanDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)
	reportDrops(spanSink, spanDrops)

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

// ставится один раз из main до горячего трафика; nil-сток — no-op.
func (w *SpanWriter) SetDropSink(fn func(orgID, n int64)) {
	w.mu.Lock()
	w.onDrop = fn
	w.mu.Unlock()
}

// вызывающий обязан слить результат через reportDrops ПОСЛЕ разблокировки.
func (w *SpanWriter) takeDropsLocked() (map[int64]int64, func(orgID, n int64)) {
	if len(w.pendingDrops) == 0 {
		return nil, w.onDrop
	}
	m := w.pendingDrops
	w.pendingDrops = nil
	return m, w.onDrop
}

// нужен в flush, где возврат неудачной пачки может переполнить txBuf уже под mu.
func (w *SpanWriter) emitDrops() {
	w.mu.Lock()
	drops, sink := w.takeDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)
}

// ставится один раз до горячего трафика; nil-сток — no-op. Отдельный от
// SetDropSink: дроп спана не квота (билится только транзакция).
func (w *SpanWriter) SetSpanDropSink(fn func(orgID, n int64)) {
	w.mu.Lock()
	w.onSpanDrop = fn
	w.mu.Unlock()
}

// вызывающий обязан слить результат через reportDrops ПОСЛЕ разблокировки.
func (w *SpanWriter) takeSpanDropsLocked() (map[int64]int64, func(orgID, n int64)) {
	if len(w.pendingSpanDrops) == 0 {
		return nil, w.onSpanDrop
	}
	m := w.pendingSpanDrops
	w.pendingSpanDrops = nil
	return m, w.onSpanDrop
}

// нужен в flush, где возврат неудачной пачки может переполнить spanBuf уже под mu.
func (w *SpanWriter) emitSpanDrops() {
	w.mu.Lock()
	drops, sink := w.takeSpanDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)
}

func reportDrops(sink func(orgID, n int64), drops map[int64]int64) {
	if sink == nil {
		return
	}
	for orgID, n := range drops {
		sink(orgID, n)
	}
}

// несериализуемое значение даёт "{}" — колонка data всегда валидный JSON.
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

// потолка по строкам одного не хватает — размер строки задаёт клиент, и
// раздутые строки могли бы разрастись до гигабайт в буфере на строки.
const defaultMaxBufBytes = 256 << 20

// без этой надбавки пустые/однобуквенные значения весили бы почти ноль, и
// байтовый потолок не срабатывал бы никогда.
const rowOverheadBytes = 64

func txRowBytes(r txRow) int64 {
	n := len(r.TraceID) + len(r.SpanID) + len(r.Transaction) + len(r.Op) +
		len(r.Status) + len(r.Environment) + len(r.Release) + len(r.ServerName) +
		len(r.UserID) + len(r.Source)
	for k, v := range r.Tags {
		n += len(k) + len(v)
	}
	for k := range r.Measurements {
		n += len(k) + 8
	}
	return int64(n) + rowOverheadBytes
}

func spanRowBytes(r spanRow) int64 {
	return int64(len(r.TraceID)+len(r.SpanID)+len(r.ParentSpanID)+
		len(r.Transaction)+len(r.Op)+len(r.Description)+len(r.Status)+
		len(r.Environment)+len(r.Data)+len(r.Source)) + rowOverheadBytes
}

// стоимость O(числа выброшенных) — вес ведётся инкрементально в Add.
func (w *SpanWriter) trimTxLocked() bool {
	drop := 0
	if over := len(w.txBuf) - w.maxBuf; over > 0 {
		drop = over
		for i := 0; i < over; i++ {
			w.txBytes -= txRowBytes(w.txBuf[i])
		}
	}
	for drop < len(w.txBuf)-1 && w.txBytes > w.maxBufBytes {
		w.txBytes -= txRowBytes(w.txBuf[drop])
		drop++
	}
	if drop <= 0 {
		return false
	}
	// без атрибуции по org потеря на буфере писателя невидима per-org (только
	// txBuf — см. pendingDrops).
	for i := 0; i < drop; i++ {
		if org := w.txBuf[i].OrgID; org > 0 {
			if w.pendingDrops == nil {
				w.pendingDrops = make(map[int64]int64)
			}
			w.pendingDrops[org]++
		}
	}
	w.txBuf = append(w.txBuf[:0], w.txBuf[drop:]...)
	w.dropped += int64(drop)
	return true
}

func (w *SpanWriter) trimSpansLocked() bool {
	drop := 0
	if over := len(w.spanBuf) - w.maxSpanBuf; over > 0 {
		drop = over
		for i := 0; i < over; i++ {
			w.spanBytes -= spanRowBytes(w.spanBuf[i])
		}
	}
	for drop < len(w.spanBuf)-1 && w.spanBytes > w.maxBufBytes {
		w.spanBytes -= spanRowBytes(w.spanBuf[drop])
		drop++
	}
	if drop <= 0 {
		return false
	}
	for i := 0; i < drop; i++ {
		if org := w.spanBuf[i].OrgID; org > 0 {
			if w.pendingSpanDrops == nil {
				w.pendingSpanDrops = make(map[int64]int64)
			}
			w.pendingSpanDrops[org]++
		}
	}
	w.spanBuf = append(w.spanBuf[:0], w.spanBuf[drop:]...)
	w.dropped += int64(drop)
	return true
}

func (w *SpanWriter) recountTxLocked() {
	w.txBytes = 0
	for i := range w.txBuf {
		w.txBytes += txRowBytes(w.txBuf[i])
	}
}

func (w *SpanWriter) recountSpansLocked() {
	w.spanBytes = 0
	for i := range w.spanBuf {
		w.spanBytes += spanRowBytes(w.spanBuf[i])
	}
}

func (w *SpanWriter) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// растущая глубина — первый признак, что хранилище не принимает.
func (w *SpanWriter) Buffered() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.txBuf) + len(w.spanBuf))
}

// максимум по четырём плечам, не сумма/среднее (иначе спановое плечо было бы
// не видно); не клампится единицей — между append и trim* потолок бывает превышен.
func (w *SpanWriter) Saturation() float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	sat := bufSaturation(int64(len(w.txBuf)), int64(w.maxBuf))
	if s := bufSaturation(int64(len(w.spanBuf)), int64(w.maxSpanBuf)); s > sat {
		sat = s
	}
	if s := bufSaturation(w.txBytes, w.maxBufBytes); s > sat {
		sat = s
	}
	if s := bufSaturation(w.spanBytes, w.maxBufBytes); s > sat {
		sat = s
	}
	return sat
}

// den<=0 — потолок выключен, «не ограничены», не «делить не на что»: не
// паникует и не показывает мнимое насыщение.
func bufSaturation(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// отличается от Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется, потеря — только при переполнении буфера.
func (w *SpanWriter) InsertFailures() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.insertFails
}

// ограничивает попытку, даже если у parent ctx нет своего дедлайна — сетевая
// чёрная дыра в PrepareBatch/Send не должна вешать Run/Close навсегда.
func (w *SpanWriter) flushWithTimeout(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	w.flush(ctx)
}

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

// ретраит с паузой, пока жив ctx, сдаётся только по ctx; идемпотентен —
// повторный вызов безопасен.
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

func (w *SpanWriter) flush(ctx context.Context) {
	w.flushTx(ctx)
	w.flushSpans(ctx)
}

func (w *SpanWriter) flushTx(ctx context.Context) {
	// возврат неудачной пачки может переполнить txBuf и вызвать trimTxLocked —
	// сливаем per-org дропы после того, как секции отпустят mu.
	defer w.emitDrops()
	w.mu.Lock()
	n := min(len(w.txBuf), w.batchSize)
	if n == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]txRow, n)
	copy(batch, w.txBuf[:n])
	w.txBuf = append(w.txBuf[:0], w.txBuf[n:]...)
	w.recountTxLocked()
	w.mu.Unlock()

	if err := w.insertTx(ctx, batch); err != nil {
		// Data-level «яд» изолируем сразу; транзиент (сеть/ctx) терпим до порога.
		poison := chbatch.IsServerDataError(err)
		w.mu.Lock()
		w.txFailStreak++
		streak := w.txFailStreak
		w.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// ядовитые дропнутся, хорошие вставятся, транзиентные вернутся в
			// буфер без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insertTx, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.insertFails++
			// сбрасываем счётчик только если изоляция что-то разрешила — иначе
			// дробление перезапускается на лежащем CH каждый тик впустую.
			if dropped > 0 || len(unresolved) < len(batch) {
				w.txFailStreak = 0
			}
			var over int
			if len(unresolved) > 0 {
				w.txBuf = append(unresolved, w.txBuf...)
				before := w.dropped
				w.recountTxLocked()
				w.trimTxLocked()
				over = int(w.dropped - before)
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
		before := w.dropped
		w.recountTxLocked()
		w.trimTxLocked()
		over := int(w.dropped - before)
		w.insertFails++
		w.mu.Unlock()
		slog.Warn("transaction batch insert failed, will retry",
			"rows", len(batch), "error", err, "dropped", over)
		return
	}
	w.mu.Lock()
	w.txFailStreak = 0
	w.mu.Unlock()
}

func (w *SpanWriter) flushSpans(ctx context.Context) {
	// возврат неудачной пачки может переполнить spanBuf и вызвать trimSpansLocked —
	// сливаем per-org дропы после того, как секции отпустят mu.
	defer w.emitSpanDrops()
	w.mu.Lock()
	n := min(len(w.spanBuf), w.spanBatchSize)
	if n == 0 {
		w.mu.Unlock()
		return
	}
	batch := make([]spanRow, n)
	copy(batch, w.spanBuf[:n])
	w.spanBuf = append(w.spanBuf[:0], w.spanBuf[n:]...)
	w.recountSpansLocked()
	w.mu.Unlock()

	if err := w.insertSpans(ctx, batch); err != nil {
		// Data-level «яд» изолируем сразу; транзиент (сеть/ctx) терпим до порога.
		poison := chbatch.IsServerDataError(err)
		w.mu.Lock()
		w.spanFailStreak++
		streak := w.spanFailStreak
		w.mu.Unlock()

		if poison || streak >= poisonThreshold {
			// ядовитые дропнутся, хорошие вставятся, транзиентные вернутся в
			// буфер без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insertSpans, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.insertFails++
			// сбрасываем счётчик только если изоляция что-то разрешила — иначе
			// дробление перезапускается на лежащем CH каждый тик впустую.
			if dropped > 0 || len(unresolved) < len(batch) {
				w.spanFailStreak = 0
			}
			var over int
			if len(unresolved) > 0 {
				w.spanBuf = append(unresolved, w.spanBuf...)
				before := w.dropped
				w.recountSpansLocked()
				w.trimSpansLocked()
				over = int(w.dropped - before)
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
		before := w.dropped
		w.recountSpansLocked()
		w.trimSpansLocked()
		over := int(w.dropped - before)
		w.insertFails++
		w.mu.Unlock()
		slog.Warn("span batch insert failed, will retry",
			"rows", len(batch), "error", err, "dropped", over)
		return
	}
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

// дефолт рассчитан на инстанс без ограничения памяти — на профиле с mem_limit
// (docker-compose.small.yml) буфер 256 МиБ не сработает раньше OOM-killer'а.
func (w *SpanWriter) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	w.maxBufBytes = n
	w.mu.Unlock()
}
