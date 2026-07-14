package event

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeConn/fakeBatch: Append копит строки во временный счётчик, Send при
// успехе переносит их в c.rows, при c.fail — возвращает ошибку.
type fakeConn struct {
	mu    sync.Mutex
	rows  int
	fail  bool
	sends int
}

type fakeBatch struct {
	conn    *fakeConn
	pending int
}

func (b *fakeBatch) Append(_ ...any) error         { b.pending++; return nil }
func (b *fakeBatch) AppendStruct(any) error        { return nil }
func (b *fakeBatch) Abort() error                  { return nil }
func (b *fakeBatch) Flush() error                  { return nil }
func (b *fakeBatch) IsSent() bool                  { return false }
func (b *fakeBatch) Rows() int                     { return b.pending }
func (b *fakeBatch) Close() error                  { return nil }
func (b *fakeBatch) Column(int) driver.BatchColumn { return nil }
func (b *fakeBatch) Columns() []column.Interface   { return nil }
func (b *fakeBatch) Send() error {
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	if b.conn.fail {
		return errors.New("ch down")
	}
	b.conn.rows += b.pending
	return nil
}

func (c *fakeConn) PrepareBatch(_ context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	c.sends++
	c.mu.Unlock()
	return &fakeBatch{conn: c}, nil
}

func TestBatcherFlushBySize(t *testing.T) {
	c := &fakeConn{}
	b := NewBatcher(c)
	b.batchSize = 10
	b.interval = time.Hour // только по размеру
	go b.Run()
	for i := 0; i < 10; i++ {
		b.Add(Event{ID: "e", ProjectID: 1, IssueID: 1, Timestamp: time.Now()})
	}
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.rows == 10 })
	_ = b.Close(context.Background())
}

func TestBatcherRetryKeepsEvents(t *testing.T) {
	c := &fakeConn{fail: true}
	b := NewBatcher(c)
	b.batchSize = 2
	b.interval = 20 * time.Millisecond
	go b.Run()
	b.Add(Event{ID: "a"})
	b.Add(Event{ID: "b"})
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.sends >= 2 }) // ретраится
	c.mu.Lock()
	c.fail = false
	c.mu.Unlock()
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.rows == 2 }) // доехали
	_ = b.Close(context.Background())
}

func TestBatcherDropsOldestOnOverflow(t *testing.T) {
	c := &fakeConn{fail: true}
	b := NewBatcher(c)
	b.maxBuf = 5
	b.interval = time.Hour
	b.batchSize = 100
	for i := 0; i < 8; i++ {
		b.Add(Event{ID: "x"})
	}
	if got := b.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3", got)
	}
	if n := len(b.buf); n != 5 {
		t.Fatalf("buffer len = %d, want 5", n)
	}
}

func TestCloseDrainsAfterTransientFailure(t *testing.T) {
	c := &fakeConn{fail: true}
	b := NewBatcher(c)
	b.interval = time.Hour
	go b.Run()
	b.Add(Event{ID: "a"})
	b.Add(Event{ID: "b"})

	go func() {
		time.Sleep(100 * time.Millisecond)
		c.mu.Lock()
		c.fail = false
		c.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close after transient failure: %v (events lost)", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rows != 2 {
		t.Fatalf("rows = %d, want 2", c.rows)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := &fakeConn{}
	b := NewBatcher(c)
	b.interval = time.Hour
	go b.Run()
	b.Add(Event{ID: "a"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not panic (close of closed channel) and must return promptly.
	if err := b.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
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
