package uptime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeCHConn/fakeCHBatch mirror event.fakeConn/fakeBatch: Append counts rows
// pending in the batch, Send moves them into conn.rows on success or fails
// when conn.fail is set.
type fakeCHConn struct {
	mu    sync.Mutex
	rows  int
	fail  bool
	sends int
	// poison — если задан, Send падает, когда среди аргументов Append есть
	// «ядовитый» ряд (data-level отказ конкретной строки, не всего батча).
	poison func(args []any) bool
}

type fakeCHBatch struct {
	conn    *fakeCHConn
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
func (b *fakeCHBatch) Send() error {
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	if b.conn.fail {
		return errors.New("ch down")
	}
	if b.conn.poison != nil {
		for _, r := range b.rows {
			if b.conn.poison(r) {
				// Серверная ошибка CH (data-level) — распознаётся как «яд».
				return &clickhouse.Exception{Code: 53, Message: "mutation error on poison row"}
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

func TestResultWriterFlushBySize(t *testing.T) {
	c := &fakeCHConn{}
	w := NewResultWriter(c)
	w.batchSize = 10
	w.interval = time.Hour // только по размеру
	go w.Run()
	for i := 0; i < 10; i++ {
		w.Add(1, 1, "local", time.Now(), Result{OK: true})
	}
	waitForCH(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.rows == 10 })
	_ = w.Close(context.Background())
}

func TestResultWriterRetryKeepsRows(t *testing.T) {
	c := &fakeCHConn{fail: true}
	w := NewResultWriter(c)
	w.batchSize = 2
	w.interval = 20 * time.Millisecond
	go w.Run()
	w.Add(1, 1, "local", time.Now(), Result{OK: true})
	w.Add(1, 1, "local", time.Now(), Result{OK: true})
	waitForCH(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.sends >= 2 }) // ретраится
	c.mu.Lock()
	c.fail = false
	c.mu.Unlock()
	waitForCH(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.rows == 2 }) // доехали
	_ = w.Close(context.Background())
}

func TestResultWriterDropsOldestOnOverflow(t *testing.T) {
	c := &fakeCHConn{fail: true}
	w := NewResultWriter(c)
	w.maxBuf = 5
	w.interval = time.Hour
	w.batchSize = 100
	for i := 0; i < 8; i++ {
		w.Add(1, 1, "local", time.Now(), Result{OK: true})
	}
	if got := w.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3", got)
	}
	if n := len(w.buf); n != 5 {
		t.Fatalf("buffer len = %d, want 5", n)
	}
}

func TestResultWriterBulkDropOnOverfilledBuffer(t *testing.T) {
	// Буфер переполнен сверх maxBuf (как после возврата пачки во flush). Один
	// Add обязан разом срезать весь избыток: при 7 рядах и maxBuf=5 добавление
	// одного элемента дропает 3 (7+1-5) и оставляет ровно maxBuf.
	c := &fakeCHConn{}
	w := NewResultWriter(c)
	w.maxBuf = 5
	w.interval = time.Hour
	w.batchSize = 100
	at := time.Now()
	for i := 0; i < 7; i++ {
		w.buf = append(w.buf, resultRow{ProjectID: 1, MonitorID: 1, Region: "local", At: at, Result: Result{OK: true}})
	}
	w.Add(1, 1, "local", at, Result{OK: true})
	if n := len(w.buf); n != 5 {
		t.Fatalf("buffer len = %d, want 5 (не должен превышать maxBuf)", n)
	}
	if got := w.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3 (bulk-сдвиг за один Add)", got)
	}
}

func TestResultWriterCloseIsIdempotent(t *testing.T) {
	c := &fakeCHConn{}
	w := NewResultWriter(c)
	w.interval = time.Hour
	go w.Run()
	w.Add(1, 1, "local", time.Now(), Result{OK: true})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestResultWriterIsolatesPoisonRowAfterThreshold(t *testing.T) {
	// Send падает, если среди рядов есть «ядовитый» (region == "poison").
	// В insert region передаётся третьим аргументом Append.
	c := &fakeCHConn{poison: func(args []any) bool {
		return len(args) > 2 && args[2] == "poison"
	}}
	w := NewResultWriter(c)
	w.batchSize = 100 // весь буфер уходит одним батчем

	at := time.Now()
	w.Add(1, 1, "poison", at, Result{OK: true}) // 1 ядовитый
	for i := 0; i < 5; i++ {                     // + 5 хороших
		w.Add(1, 1, "local", at, Result{OK: true})
	}

	// Больше порога подряд-фейлов: обычный ретрай застревает на ядовитом ряду,
	// после poisonThreshold флаш должен изолировать его бинарным дроблением.
	for i := 0; i < poisonThreshold+1; i++ {
		w.flush(context.Background())
	}

	if n := len(w.buf); n != 0 {
		t.Fatalf("буфер должен дренироваться после изоляции яда, осталось %d", n)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1 (ядовитый ряд)", got)
	}
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != 5 {
		t.Fatalf("вставлено хороших рядов = %d, want 5", rows)
	}
}

// Транзиентный отказ (сеть/ctx): изоляция не должна дропать валидные результаты.
func TestResultWriterTransientFailureDropsNothing(t *testing.T) {
	c := &fakeCHConn{fail: true}
	w := NewResultWriter(c)
	w.batchSize = 100
	at := time.Now()
	for i := 0; i < 4; i++ {
		w.Add(1, 1, "local", at, Result{OK: true})
	}
	for i := 0; i < poisonThreshold+3; i++ {
		w.flush(context.Background())
	}
	if n := len(w.buf); n != 4 {
		t.Fatalf("буфер = %d, want 4 (ряды остаются на ретрай)", n)
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
