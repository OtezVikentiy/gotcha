package event

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

// fakeConn/fakeBatch: Append копит строки во временный счётчик, Send при
// успехе переносит их в c.rows, при c.fail — возвращает ошибку.
type fakeConn struct {
	mu     sync.Mutex
	rows   int
	fail   bool
	sends  int
	poison func(projectID uint64) bool // если задан и в батче есть ядовитый ряд — Send падает
}

type fakeBatch struct {
	conn     *fakeConn
	pending  int
	projects []uint64 // project_id каждого добавленного ряда (для poison-предиката)
}

func (b *fakeBatch) Append(args ...any) error {
	b.pending++
	if len(args) > 1 {
		if pid, ok := args[1].(uint64); ok {
			b.projects = append(b.projects, pid)
		}
	}
	return nil
}
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
	if b.conn.poison != nil {
		for _, pid := range b.projects {
			if b.conn.poison(pid) {
				// Серверная ошибка CH (data-level) — распознаётся как «яд».
				return &clickhouse.Exception{Code: 53, Message: "type mismatch"}
			}
		}
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

func TestBatcherBulkDropOnOverfilledBuffer(t *testing.T) {
	// Буфер переполнен сверх maxBuf (как после возврата пачки в буфер во flush).
	// Один Add обязан разом срезать весь избыток (bulk-сдвиг), а не по одному:
	// при 7 рядах и maxBuf=5 добавление одного элемента должно дропнуть 3
	// (7+1-5) и оставить ровно maxBuf.
	c := &fakeConn{}
	b := NewBatcher(c)
	b.maxBuf = 5
	b.interval = time.Hour
	b.batchSize = 100
	for i := 0; i < 7; i++ {
		b.buf = append(b.buf, Event{ID: "x"})
	}
	b.Add(Event{ID: "new"})
	if n := len(b.buf); n != 5 {
		t.Fatalf("buffer len = %d, want 5 (не должен превышать maxBuf)", n)
	}
	if got := b.Dropped(); got != 3 {
		t.Fatalf("Dropped() = %d, want 3 (bulk-сдвиг за один Add)", got)
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

func TestBatcherIsolatesPoisonRowAfterThreshold(t *testing.T) {
	// conn.Send падает, если среди рядов есть событие ядовитого проекта.
	const poisonPID = 999
	c := &fakeConn{poison: func(pid uint64) bool { return pid == poisonPID }}
	b := NewBatcher(c)
	b.Add(Event{ID: "poison", ProjectID: poisonPID, Timestamp: time.Now()})
	for i := 0; i < 5; i++ {
		b.Add(Event{ID: "ok", ProjectID: 1, Timestamp: time.Now()})
	}
	// Прогоняем flush больше порога: обычный ретрай застревает на ядовитом ряду,
	// после poisonThreshold подряд-фейлов должна сработать изоляция.
	for i := 0; i < poisonThreshold+1; i++ {
		b.flush(context.Background())
	}
	b.mu.Lock()
	buffered := len(b.buf)
	b.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("buffer should drain after poison isolation, still %d buffered", buffered)
	}
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != 5 {
		t.Fatalf("want 5 good rows inserted, got %d", rows)
	}
	if got := b.Dropped(); got != 1 {
		t.Fatalf("want 1 dropped poison row, got %d", got)
	}
}

// Транзиентный отказ (сеть/ctx, НЕ *clickhouse.Exception): даже после порога
// подряд-фейлов изоляция НЕ должна дропать валидные ряды — они возвращаются в
// буфер под обычный ретрай. Dropped() обязан остаться 0.
func TestBatcherTransientFailureDropsNothing(t *testing.T) {
	c := &fakeConn{fail: true} // Send всегда возвращает обычную errors.New — транзиент
	b := NewBatcher(c)
	b.Add(Event{ID: "a", ProjectID: 1, Timestamp: time.Now()})
	b.Add(Event{ID: "b", ProjectID: 1, Timestamp: time.Now()})

	for i := 0; i < poisonThreshold+3; i++ {
		b.flush(context.Background())
	}

	if got := b.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (транзиент не должен ничего терять)", got)
	}
	b.mu.Lock()
	buffered := len(b.buf)
	b.mu.Unlock()
	if buffered != 2 {
		t.Fatalf("buffered = %d, want 2 (ряды остаются на ретрай)", buffered)
	}
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != 0 {
		t.Fatalf("inserted rows = %d, want 0 (CH недоступен)", rows)
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
