package uptime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
}

type fakeCHBatch struct {
	conn    *fakeCHConn
	pending int
}

func (b *fakeCHBatch) Append(_ ...any) error         { b.pending++; return nil }
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
