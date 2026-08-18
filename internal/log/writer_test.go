package log

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

// fakeCHConn/fakeCHBatch — калька internal/metric/writer_unit_test.go: Append
// копит строки, Send при успехе переносит их в c.rows, а при заданном
// poison-предикате падает, если в батче есть ряд с ядовитым body (args[6] в
// insert — это Body).
type fakeCHConn struct {
	mu     sync.Mutex
	rows   int
	sends  int
	fail   bool // если true — Send падает транзиентной (не серверной) ошибкой
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

func (c *fakeCHConn) PrepareBatch(_ context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	c.sends++
	c.mu.Unlock()
	return &fakeCHBatch{conn: c}, nil
}

// TestWriterFlushesAddedRowsInOrder — Add N записей → после flush в
// fake-conn все N строк; Add не блокирует (буфер копится синхронно, кика
// ждать не нужно, flush вызываем напрямую).
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

// TestWriterIsolatesPoisonRowAfterThreshold — битая строка изолируется через
// chbatch.IsolatePoison, остальные проходят.
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

// Транзиентный отказ (сеть/ctx): изоляция не должна дропать валидные записи.
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

// TestWriterBoundsBufferByBytes — буфер логов был бы ограничен только ЧИСЛОМ
// строк, а размер строки задаёт клиент: body лога доходит до 64 КиБ на
// запись. maxBuf раздутых строк с большим body — это гигабайты в буфере.
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
	// Учёт не разъехался с содержимым.
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

// TestLogRowBytesCountsBody — защита от того, что body (текст сообщения,
// до 64 КиБ) не попадёт в вес и trimLocked никогда не сработает: строка с
// большим body должна быть тяжелее пропорционально длине body.
func TestLogRowBytesCountsBody(t *testing.T) {
	a := logRowBytes(logRow{Severity: "info"})
	b := logRowBytes(logRow{Severity: "info", Body: strings.Repeat("x", 1000)})
	if b-a != 1000 {
		t.Fatalf("body weight = %d, want 1000", b-a)
	}
}
