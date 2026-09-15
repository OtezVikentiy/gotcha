package log

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Калька internal/metric/writer_unit_test.go: Append копит строки, Send при успехе переносит их в c.rows,
// при заданном poison-предикате падает, если в батче есть ряд с ядовитым body (args[6] в insert — Body).
type fakeCHConn struct {
	mu     sync.Mutex
	rows   int
	sends  int
	fail   bool // если true — Send падает транзиентной (не серверной) ошибкой
	badCtx bool // BatchContext не пришёл: Done() != nil или Deadline() не взведён
	poison func(body string) bool
}

type fakeCHBatch struct {
	conn    *fakeCHConn
	pending int
	bodies  []string
}

func (b *fakeCHBatch) Append(args ...any) error {
	b.pending++
	if len(args) > 6 {
		if v, ok := args[6].(string); ok {
			b.bodies = append(b.bodies, v)
		}
	}
	return nil
}
func (b *fakeCHBatch) AppendStruct(any) error        { return nil }
func (b *fakeCHBatch) Abort() error                  { return nil }
func (b *fakeCHBatch) Flush() error                  { return nil }
func (b *fakeCHBatch) IsSent() bool                  { return false }
func (b *fakeCHBatch) Rows() int                     { return b.pending }
func (b *fakeCHBatch) Close() error                  { return nil }
func (b *fakeCHBatch) Column(int) driver.BatchColumn { return nil }
func (b *fakeCHBatch) Columns() []column.Interface   { return nil }
func (b *fakeCHBatch) Send() error {
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	if b.conn.fail {
		return errors.New("ch down") // транзиент: не *clickhouse.Exception
	}
	if b.conn.poison != nil {
		for _, body := range b.bodies {
			if b.conn.poison(body) {
				// Серверная ошибка CH (data-level) — распознаётся как «яд».
				return &clickhouse.Exception{Code: 53, Message: "type mismatch"}
			}
		}
	}
	b.conn.rows += b.pending
	return nil
}

func (c *fakeCHConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	c.sends++
	if _, ok := ctx.Deadline(); ctx.Done() != nil || !ok {
		c.badCtx = true
	}
	c.mu.Unlock()
	return &fakeCHBatch{conn: c}, nil
}

// flushDetached обязан заворачивать ctx в db.BatchContext и на тике Run, и на закрытии — Done()
// == nil одного Background() мало (тавтология): проверяем ещё и взведённый Deadline().
func TestFlushDetachedContextNotCancelableViaRun(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.interval = 20 * time.Millisecond
	go w.Run()
	now := time.Now().UTC()
	w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "line"})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		sends := c.sends
		c.mu.Unlock()
		if sends > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = w.Close(context.Background())

	c.mu.Lock()
	sawBadCtx := c.badCtx
	c.mu.Unlock()
	if sawBadCtx {
		t.Fatal("PrepareBatch увидел не-BatchContext через тик Run (Done() != nil либо Deadline() не взведён)")
	}
}

func TestFlushDetachedContextNotCancelableViaClose(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.interval = time.Hour
	go w.Run()
	now := time.Now().UTC()
	w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "line"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c.mu.Lock()
	sawBadCtx := c.badCtx
	c.mu.Unlock()
	if sawBadCtx {
		t.Fatal("PrepareBatch увидел не-BatchContext через Close/closeDrain (Done() != nil либо Deadline() не взведён)")
	}
}

// flush вызывается напрямую — кика (async trigger) ждать не нужно, буфер копится синхронно.
func TestWriterFlushesAddedRowsInOrder(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "line"})
	}
	if got := w.Buffered(); got != 5 {
		t.Fatalf("Buffered() = %d, want 5 (Add не блокирует и копит)", got)
	}
	w.flush(context.Background())
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != 5 {
		t.Fatalf("want 5 rows inserted, got %d", rows)
	}
	if got := w.Buffered(); got != 0 {
		t.Fatalf("Buffered() = %d, want 0 after flush", got)
	}
}

func TestWriterIsolatesPoisonRowAfterThreshold(t *testing.T) {
	c := &fakeCHConn{poison: func(body string) bool { return body == "poison" }}
	w := NewWriter(c)
	now := time.Now().UTC()
	w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "error", Body: "poison"})
	for i := 0; i < 5; i++ {
		w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "ok"})
	}
	// Прогоняем flush больше порога: обычный ретрай застревает на ядовитом ряду,
	// после poisonThreshold подряд-фейлов должна сработать изоляция.
	for i := 0; i < poisonThreshold+1; i++ {
		w.flush(context.Background())
	}
	if got := w.Buffered(); got != 0 {
		t.Fatalf("buffer should drain after poison isolation, still %d buffered", got)
	}
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != 5 {
		t.Fatalf("want 5 good rows inserted, got %d", rows)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("want 1 dropped poison row, got %d", got)
	}
}

func TestWriterTransientFailureDropsNothing(t *testing.T) {
	c := &fakeCHConn{fail: true}
	w := NewWriter(c)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "ok"})
	}
	for i := 0; i < poisonThreshold+3; i++ {
		w.flush(context.Background())
	}
	if got := w.Buffered(); got != 4 {
		t.Fatalf("Buffered() = %d, want 4 (ряды остаются на ретрай)", got)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (транзиент не должен ничего терять)", got)
	}
}

// После всплеска Writer обязан слить остаток самокиками, не дожидаясь
// следующего 5с тика: Add() кикает только на переходе через batchSize.
func TestWriterDrainsBurstWithoutWaitingForTick(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.batchSize = 100

	now := time.Now().UTC()
	const burst = 2500
	for i := 0; i < burst; i++ {
		w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "line"})
	}

	ctx := context.Background()
	flushes := 0
drain:
	for {
		select {
		case <-w.kick:
			w.flush(ctx)
			flushes++
		default:
			break drain
		}
	}

	if got := w.Buffered(); got != 0 {
		t.Fatalf("Buffered = %d после %d флашей — не самокикнулся до опустошения", got, flushes)
	}
	if want := burst / w.batchSize; flushes != want {
		t.Fatalf("флашей = %d, want %d — на всплеск не хватило self-kick'ов", flushes, want)
	}
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != burst {
		t.Fatalf("вставлено %d строк, want %d", rows, burst)
	}
}

// Буфер был бы ограничен только ЧИСЛОМ строк, а размер строки задаёт клиент (body до 64 КиБ) —
// maxBuf раздутых строк с большим body это гигабайты.
func TestWriterBoundsBufferByBytes(t *testing.T) {
	w := NewWriter(nil)
	w.maxBufBytes = 1 << 20
	w.batchSize = 1 << 30

	big := strings.Repeat("a", 256<<10)
	for i := 0; i < 20; i++ {
		w.Add(1, LogRecord{Body: big})
	}

	w.mu.Lock()
	rows, bytes, dropped := len(w.buf), w.bufBytes, w.dropped
	w.mu.Unlock()

	if rows >= 20 {
		t.Fatalf("в буфере %d строк из 20 — байтовый потолок не сработал", rows)
	}
	if dropped == 0 {
		t.Fatal("ничего не выброшено, хотя буфер переполнен по байтам")
	}
	if limit := w.maxBufBytes + int64(len(big)) + 256; bytes > limit {
		t.Fatalf("вес буфера %d при потолке %d", bytes, w.maxBufBytes)
	}
	w.mu.Lock()
	var want int64
	for i := range w.buf {
		want += logRowBytes(w.buf[i])
	}
	got := w.bufBytes
	w.mu.Unlock()
	if got != want {
		t.Fatalf("bufBytes = %d, фактический вес %d — учёт разъехался", got, want)
	}
}

// Body (до 64 КиБ) обязан попадать в вес, иначе trimLocked никогда не сработает — строка с большим body
// должна быть тяжелее пропорционально длине body.
func TestLogRowBytesCountsBody(t *testing.T) {
	a := logRowBytes(logRow{Severity: "info"})
	b := logRowBytes(logRow{Severity: "info", Body: strings.Repeat("x", 1000)})
	if b-a != 1000 {
		t.Fatalf("body weight = %d, want 1000", b-a)
	}
}

func captureWarnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Close молчал об итоговых потерях, в отличие от event/trace/uptime — на
// выключенном инстансе self-метрику Dropped уже не снять, лог был единственным следом.
func TestWriterCloseLogsFinalDrops(t *testing.T) {
	buf := captureWarnLog(t)
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.maxBuf = 2
	w.batchSize = 1 << 30 // не флашим по наполнению — дроп только от переполнения буфера

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		w.Add(1, LogRecord{Timestamp: now, ObservedTS: now, Severity: "info", Body: "x"})
	}
	if w.Dropped() == 0 {
		t.Fatal("подготовка сценария сломана: дропов нет, Close нечего логировать")
	}

	go w.Run()
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !strings.Contains(buf.String(), "dropped during lifetime") {
		t.Errorf("Close не залогировал итоговые потери: %q", buf.String())
	}
}
