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
	// OrgID — организация проекта. В CH НЕ пишется; нужен только для per-org
	// атрибуции дропов txBuf в org_usage.dropped_transactions (см.
	// SpanWriter.SetDropSink). 0 — атрибутировать некуда.
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
	txBytes     int64 // приблизительный вес txBuf, см. maxBufBytes
	spanBuf     []spanRow
	spanBytes   int64 // приблизительный вес spanBuf
	dropped     int64
	insertFails int64 // накопительно: сколько флашей провалилось
	lastDropLog time.Time
	// Два независимых батча (transactions и spans) → два раздельных счётчика
	// подряд-фейлов: изоляция ядовитых рядов включается по каждой таблице отдельно.
	txFailStreak   int
	spanFailStreak int
	// pendingDrops — выброшенные с прошлого слива ТРАНЗАКЦИИ по orgID, для per-org
	// атрибуции в org_usage.dropped_transactions (см. onDrop/SetDropSink). Только
	// txBuf: транзакция — квота/биллинг-единица; дроп дочернего спана из spanBuf —
	// потеря данных, но не «дропнутая транзакция», в счётчик не идёт. nil, пока
	// дропов нет. Заполняется под mu, сливается ВНЕ mu (emitDrops).
	pendingDrops map[int64]int64
	onDrop       func(orgID, n int64) // сток per-org дропов txBuf; nil — no-op. Под mu.

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

// Add кладёт транзакцию в буферы: 1 строка в transactions и len(Spans)+1
// строк в spans (корневой спан тоже попадает в spans). Никогда не блокирует и
// не возвращает ошибку: приём транзакций не должен зависеть от здоровья
// ClickHouse.
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
	// Per-org дропы транзакций и сток захватываем под mu, сливаем — вне (сток
	// берёт свой мьютекс).
	drops, sink := w.takeDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)

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

// SetDropSink задаёт сток per-org дропов txBuf (см. SpanWriter.onDrop). Ставится
// один раз из main до горячего трафика; nil-сток — no-op.
func (w *SpanWriter) SetDropSink(fn func(orgID, n int64)) {
	w.mu.Lock()
	w.onDrop = fn
	w.mu.Unlock()
}

// takeDropsLocked забирает накопленные per-org дропы транзакций и текущий сток.
// Под mu; вызывающий сливает через reportDrops ПОСЛЕ разблокировки.
func (w *SpanWriter) takeDropsLocked() (map[int64]int64, func(orgID, n int64)) {
	if len(w.pendingDrops) == 0 {
		return nil, w.onDrop
	}
	m := w.pendingDrops
	w.pendingDrops = nil
	return m, w.onDrop
}

// emitDrops сливает накопленные per-org дропы в сток. Для flush, где возврат
// провалившейся пачки транзакций может переполнить txBuf уже под mu.
func (w *SpanWriter) emitDrops() {
	w.mu.Lock()
	drops, sink := w.takeDropsLocked()
	w.mu.Unlock()
	reportDrops(sink, drops)
}

// reportDrops вызывает сток по одному разу на организацию. sink==nil или пустая
// карта — no-op.
func reportDrops(sink func(orgID, n int64), drops map[int64]int64) {
	if sink == nil {
		return
	}
	for orgID, n := range drops {
		sink(orgID, n)
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

// defaultMaxBufBytes — потолок КАЖДОГО из буферов по байтам, в дополнение к
// потолку по строкам. Одного потолка по строкам не хватает: размер строки задаёт
// клиент (описание спана, теги транзакции, JSON data), и maxSpanBuf=100000
// раздутых строк — это десятки гигабайт в буфере, заведённом под сто тысяч
// небольших спанов. На обычном трафике первым срабатывает потолок по строкам.
const defaultMaxBufBytes = 256 << 20

// rowOverheadBytes — постоянная цена ОДНОЙ строки в буфере помимо длины строк:
// заголовки string (16 байт каждый), элемент среза, служебные поля. Без неё
// учёт был обходим тем же приёмом, что и бюджет профилей: строка из пустых или
// однобуквенных значений весила бы почти ноль, и байтовый потолок не срабатывал
// бы никогда — работал бы только счётный.
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

// trimTxLocked/trimSpansLocked приводят буфер к обоим потолкам, выбрасывая самое
// старое. Стоимость — O(числа выброшенных): вес ведётся инкрементально в Add.
// Вызываются под mu.
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
	// Списываем выброшенные транзакции их организациям (per-org атрибуция потерь
	// в org_usage.dropped_transactions): без этого потеря на слое буфера писателя
	// невидима per-org. Только txBuf — см. докблок pendingDrops.
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
	w.spanBuf = append(w.spanBuf[:0], w.spanBuf[drop:]...)
	w.dropped += int64(drop)
	return true
}

// recountTxLocked/recountSpansLocked пересчитывают вес с нуля — нужны там, где
// буфер перестраивается целиком (возврат пачки после неудачной вставки).
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

// Dropped — сколько строк (транзакций и спанов) выброшено из-за переполнения
// буферов.
func (w *SpanWriter) Dropped() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Buffered — сколько строк ждёт записи прямо сейчас: транзакции и спаны
// суммарно (у писателя два независимых буфера). Для самотелеметрии: растущая
// глубина — первый признак, что хранилище не принимает.
func (w *SpanWriter) Buffered() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.txBuf) + len(w.spanBuf))
}

// InsertFailures — сколько флашей провалилось за время жизни процесса.
// Отличается от Dropped: неудачная вставка возвращает пачку в буфер и
// повторяется, потеря наступает только при переполнении буфера.
func (w *SpanWriter) InsertFailures() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.insertFails
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
	// Возврат провалившейся пачки транзакций может переполнить txBuf и вызвать
	// trimTxLocked — сливаем per-org дропы после того, как секции отпустят mu.
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
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved (обратно в буфер) без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insertTx, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.insertFails++
			// Сбрасываем счётчик подряд-фейлов ТОЛЬКО если изоляция что-то
			// разрешила: иначе при лежащем ClickHouse дробление запускается
			// заново каждые ~15 с, не дав ничего в прошлый раз.
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
			// Изолируем: ядовитые ряды дропнутся, хорошие вставятся, транзиентные
			// вернутся в unresolved (обратно в буфер) без потерь.
			dropped, unresolved := chbatch.IsolatePoison(ctx, batch, w.insertSpans, chbatch.IsServerDataError)
			w.mu.Lock()
			w.dropped += int64(dropped)
			w.insertFails++
			// Сбрасываем счётчик подряд-фейлов ТОЛЬКО если изоляция что-то
			// разрешила: иначе при лежащем ClickHouse дробление запускается
			// заново каждые ~15 с, не дав ничего в прошлый раз.
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

// SetMaxBufferBytes задаёт байтовый потолок буфера. Значение по умолчанию
// (defaultMaxBufBytes) рассчитано на инстанс без ограничения памяти; на
// стеснённом профиле (docker-compose.small.yml: mem_limit 256m) буферы по
// 256 МиБ физически не могут сработать раньше OOM-killer'а, то есть защита
// инертна ровно там, где нужнее всего. Ставится из main по
// GOTCHA_MAX_WRITER_BUFFER_BYTES. Нулевое и отрицательное значение игнорируется.
func (w *SpanWriter) SetMaxBufferBytes(n int64) {
	if n <= 0 {
		return
	}
	w.mu.Lock()
	w.maxBufBytes = n
	w.mu.Unlock()
}
