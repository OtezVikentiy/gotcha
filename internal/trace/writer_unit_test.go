package trace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeCHConn/fakeCHBatch повторяют event.fakeConn/fakeBatch: Append копит
// строки, Send переносит их в conn.rows[таблица] либо падает, если для этой
// таблицы взведён failTx/failSpans.
type fakeCHConn struct {
	mu        sync.Mutex
	txRows    int
	spanRows  int
	txSends   int
	spanSends int
	failTx    bool
	failSpans bool
	// poisonTx/poisonSpans — предикаты «ядовитого» ряда по аргументам Append.
	// Если заданы, Send падает, когда в батче есть хотя бы один такой ряд (для
	// тестов изоляции poison-row); одиночная вставка ядовитого ряда всегда падает.
	poisonTx    func(args []any) bool
	poisonSpans func(args []any) bool
}

type fakeCHBatch struct {
	conn    *fakeCHConn
	spans   bool
	pending int
	rows    [][]any
}

func (b *fakeCHBatch) Append(args ...any) error {
	b.pending++
	b.rows = append(b.rows, args)
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

// hasPoison — есть ли в накопленном батче ряд, забракованный предикатом.
func (b *fakeCHBatch) hasPoison(pred func([]any) bool) bool {
	if pred == nil {
		return false
	}
	for _, r := range b.rows {
		if pred(r) {
			return true
		}
	}
	return false
}

func (b *fakeCHBatch) Send() error {
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	if b.spans {
		if b.conn.failSpans {
			return errors.New("ch down") // транзиент: не *clickhouse.Exception
		}
		if b.hasPoison(b.conn.poisonSpans) {
			// Серверная ошибка CH (data-level) — распознаётся как «яд».
			return &clickhouse.Exception{Code: 53, Message: "type mismatch"}
		}
		b.conn.spanRows += b.pending
		return nil
	}
	if b.conn.failTx {
		return errors.New("ch down") // транзиент: не *clickhouse.Exception
	}
	if b.hasPoison(b.conn.poisonTx) {
		return &clickhouse.Exception{Code: 53, Message: "type mismatch"}
	}
	b.conn.txRows += b.pending
	return nil
}

func (c *fakeCHConn) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	spans := strings.Contains(query, "INTO spans")
	c.mu.Lock()
	if spans {
		c.spanSends++
	} else {
		c.txSends++
	}
	c.mu.Unlock()
	return &fakeCHBatch{conn: c, spans: spans}, nil
}

func sampleTx(children int) Transaction {
	start := time.Now().UTC()
	tr := Transaction{
		TraceID: "t", SpanID: "root", Name: "GET /", Op: "http.server", Status: "ok",
		Start: start, End: start.Add(time.Millisecond), Source: "sentry",
	}
	for i := 0; i < children; i++ {
		tr.Spans = append(tr.Spans, Span{
			SpanID: "c", ParentSpanID: "root", Op: "db.query", Description: "SELECT 1",
			Start: start, End: start.Add(time.Millisecond), Status: "ok",
		})
	}
	return tr
}

func TestSpanWriterAddBuffersRootSpanInBothTables(t *testing.T) {
	w := NewSpanWriter(&fakeCHConn{})
	w.interval = time.Hour
	w.Add(1, sampleTx(3))

	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.txBuf) != 1 {
		t.Fatalf("txBuf = %d, want 1", len(w.txBuf))
	}
	if len(w.spanBuf) != 4 {
		t.Fatalf("spanBuf = %d, want 4 (3 children + root)", len(w.spanBuf))
	}
}

func TestSpanWriterFlushBySize(t *testing.T) {
	c := &fakeCHConn{}
	w := NewSpanWriter(c)
	w.batchSize = 10
	w.interval = time.Hour // только по размеру
	go w.Run()
	for i := 0; i < 10; i++ {
		w.Add(1, sampleTx(1))
	}
	waitForCH(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.txRows == 10 && c.spanRows == 20
	})
	_ = w.Close(context.Background())
}

func TestSpanWriterRetryKeepsRows(t *testing.T) {
	c := &fakeCHConn{failTx: true, failSpans: true}
	w := NewSpanWriter(c)
	w.batchSize = 2
	w.interval = 20 * time.Millisecond
	go w.Run()
	w.Add(1, sampleTx(1))
	w.Add(1, sampleTx(1))
	waitForCH(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.txSends >= 2 }) // ретраится
	c.mu.Lock()
	c.failTx, c.failSpans = false, false
	c.mu.Unlock()
	waitForCH(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.txRows == 2 && c.spanRows == 4
	})
	_ = w.Close(context.Background())
}

// Падение вставки в одну таблицу не должно ни терять, ни дублировать строки
// другой: буферы независимы.
func TestSpanWriterSpanFailureDoesNotResendTransactions(t *testing.T) {
	c := &fakeCHConn{failSpans: true}
	w := NewSpanWriter(c)
	w.batchSize = 1
	w.interval = 20 * time.Millisecond
	go w.Run()
	w.Add(1, sampleTx(1))
	waitForCH(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.txRows == 1 && c.spanSends >= 2 })
	c.mu.Lock()
	c.failSpans = false
	c.mu.Unlock()
	waitForCH(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.spanRows == 2 })
	if err := w.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txRows != 1 {
		t.Fatalf("txRows = %d, want 1 (транзакция не должна вставляться дважды)", c.txRows)
	}
}

func TestSpanWriterDropsOldestOnOverflow(t *testing.T) {
	c := &fakeCHConn{failTx: true, failSpans: true}
	w := NewSpanWriter(c)
	w.maxBuf = 5
	w.maxSpanBuf = 5
	w.interval = time.Hour
	w.batchSize = 100
	w.spanBatchSize = 100
	for i := 0; i < 8; i++ {
		w.Add(1, sampleTx(0)) // по 1 строке в каждый буфер
	}
	if got := w.Dropped(); got != 6 { // 3 транзакции + 3 спана
		t.Fatalf("Dropped() = %d, want 6", got)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.txBuf) != 5 || len(w.spanBuf) != 5 {
		t.Fatalf("buffers: tx=%d spans=%d, want 5/5", len(w.txBuf), len(w.spanBuf))
	}
}

func TestSpanWriterCloseFlushesAndIsIdempotent(t *testing.T) {
	c := &fakeCHConn{}
	w := NewSpanWriter(c)
	w.interval = time.Hour
	go w.Run()
	w.Add(1, sampleTx(2))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txRows != 1 || c.spanRows != 3 {
		t.Fatalf("after Close: txRows=%d spanRows=%d, want 1/3", c.txRows, c.spanRows)
	}
}

// txNamed — транзакция без дочерних спанов с заданным именем (колонка
// transaction). Даёт ровно 1 строку в txBuf и 1 корневой спан в spanBuf.
func txNamed(name string) Transaction {
	start := time.Now().UTC()
	return Transaction{
		TraceID: "t", SpanID: "root", Name: name, Op: "http.server", Status: "ok",
		Start: start, End: start.Add(time.Millisecond), Source: "sentry",
	}
}

// txWithChildDescs — транзакция (root Description = "ok") с дочерними спанами
// заданных описаний. Позволяет подложить «ядовитый» спан в spanBuf.
func txWithChildDescs(descs ...string) Transaction {
	tr := txNamed("ok")
	for _, d := range descs {
		tr.Spans = append(tr.Spans, Span{
			SpanID: "c", ParentSpanID: "root", Op: "db.query", Description: d,
			Start: tr.Start, End: tr.End, Status: "ok",
		})
	}
	return tr
}

// После poisonThreshold подряд-фейлов flushTx обязан изолировать ядовитый tx-ряд
// (бинарное дробление): хорошие транзакции вставляются, ядовитая дропается,
// txBuf разблокируется — head-of-line blocking снят.
func TestSpanWriterIsolatesPoisonTxRowAfterThreshold(t *testing.T) {
	// insertTx падает, если в батче есть транзакция с именем "poison"
	// (args[3] — колонка transaction, см. порядок Append в insertTx).
	c := &fakeCHConn{poisonTx: func(args []any) bool {
		return len(args) > 3 && args[3] == "poison"
	}}
	w := NewSpanWriter(c)
	w.interval = time.Hour // без авто-флаша: гоняем flushTx вручную

	w.Add(1, txNamed("poison"))
	for i := 0; i < 5; i++ {
		w.Add(1, txNamed("ok"))
	}
	// txBuf: 1 ядовитый + 5 хороших (всё в одном батче, batchSize=1000).

	for i := 0; i < poisonThreshold+1; i++ {
		w.flushTx(context.Background())
	}

	w.mu.Lock()
	txLeft := len(w.txBuf)
	w.mu.Unlock()
	if txLeft != 0 {
		t.Fatalf("txBuf should drain after poison isolation, still %d buffered", txLeft)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1 (ядовитая транзакция)", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.txRows != 5 {
		t.Fatalf("inserted tx rows = %d, want 5 (хорошие)", c.txRows)
	}
}

// То же для батча спанов: раздельный счётчик spanFailStreak и изоляция в
// flushSpans, независимо от tx.
func TestSpanWriterIsolatesPoisonSpanRowAfterThreshold(t *testing.T) {
	// insertSpans падает, если в батче есть спан с описанием "poison"
	// (args[6] — колонка description, см. порядок Append в insertSpans).
	c := &fakeCHConn{poisonSpans: func(args []any) bool {
		return len(args) > 6 && args[6] == "poison"
	}}
	w := NewSpanWriter(c)
	w.interval = time.Hour

	w.Add(1, txWithChildDescs("poison")) // root(ok) + child(poison)
	for i := 0; i < 5; i++ {
		w.Add(1, txNamed("ok")) // root(ok)
	}
	// spanBuf: 1 ядовитый + 6 хороших (root'ы).

	for i := 0; i < poisonThreshold+1; i++ {
		w.flushSpans(context.Background())
	}

	w.mu.Lock()
	spanLeft := len(w.spanBuf)
	w.mu.Unlock()
	if spanLeft != 0 {
		t.Fatalf("spanBuf should drain after poison isolation, still %d buffered", spanLeft)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1 (ядовитый спан)", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.spanRows != 6 {
		t.Fatalf("inserted span rows = %d, want 6 (хорошие)", c.spanRows)
	}
}

// Транзиентный отказ обеих таблиц (сеть/ctx): даже после порога подряд-фейлов
// изоляция не должна дропать валидные tx/span-ряды — они остаются в буфере.
func TestSpanWriterTransientFailureDropsNothing(t *testing.T) {
	c := &fakeCHConn{failTx: true, failSpans: true}
	w := NewSpanWriter(c)
	w.interval = time.Hour
	for i := 0; i < 4; i++ {
		w.Add(1, txNamed("ok")) // 1 tx + 1 root-span на каждый Add
	}
	for i := 0; i < poisonThreshold+3; i++ {
		w.flushTx(context.Background())
		w.flushSpans(context.Background())
	}
	w.mu.Lock()
	txLeft, spanLeft := len(w.txBuf), len(w.spanBuf)
	w.mu.Unlock()
	if txLeft != 4 || spanLeft != 4 {
		t.Fatalf("buffers: tx=%d spans=%d, want 4/4 (ряды остаются на ретрай)", txLeft, spanLeft)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (транзиент не должен ничего терять)", got)
	}
}

func waitForCH(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in 5s")
}
