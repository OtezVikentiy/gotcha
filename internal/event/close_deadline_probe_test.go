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

type slowConn struct {
	mu    sync.Mutex
	sends int
	delay time.Duration
}

type slowBatch struct {
	c   *slowConn
	ctx context.Context
}

func (b *slowBatch) Append(args ...any) error      { return nil }
func (b *slowBatch) AppendStruct(any) error        { return nil }
func (b *slowBatch) Abort() error                  { return nil }
func (b *slowBatch) Flush() error                  { return nil }
func (b *slowBatch) IsSent() bool                  { return false }
func (b *slowBatch) Rows() int                     { return 1 }
func (b *slowBatch) Close() error                  { return nil }
func (b *slowBatch) Column(int) driver.BatchColumn { return nil }
func (b *slowBatch) Columns() []column.Interface   { return nil }

// Как настоящий драйвер: отменённый ctx обрывает вставку. db.BatchContext такой ctx не даёт —
// эта ветка в проде недостижима, но фейк обязан вести себя как conn_batch.go Send().
func (b *slowBatch) Send() error {
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case <-time.After(b.c.delay):
		return nil
	}
}

func (c *slowConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sends++
	c.mu.Unlock()
	return &slowBatch{c: c, ctx: ctx}, nil
}

// closeDrain обязан уйти по ctx.Err(), не дренировать буфер целиком: проверку ctx.Done() между
// флашами держит верхний select цикла, не только «флаш не продвинулся».
func TestCloseHonoursDeadlineDespiteSlowFlushes(t *testing.T) {
	c := &slowConn{delay: 100 * time.Millisecond}
	b := NewBatcher(c)
	b.batchSize = 10
	b.interval = time.Hour
	go b.Run()
	for i := 0; i < 500; i++ {
		b.Add(Event{ProjectID: 1})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	c.mu.Lock()
	sendsBefore := c.sends
	c.mu.Unlock()

	start := time.Now()
	err := b.Close(ctx)
	elapsed := time.Since(start)

	c.mu.Lock()
	flushesDuringClose := c.sends - sendsBefore
	c.mu.Unlock()
	t.Logf("Close returned after %v, err=%v, flushes during Close=%d", elapsed, err, flushesDuringClose)

	// Нагрузконезависимый различитель: сломанный closeDrain дренирует буфер целиком и вернёт nil.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close err = %v, want context.DeadlineExceeded", err)
	}
	// Запас x3 в обе стороны: на исправленном коде 2-7 флашей за время Close, на сломанном — ~50
	// (дренаж буфера целиком). Времени здесь нарочно нет — порог по elapsed квантован числом
	// флашей, успевших проскочить до проверки отмены, и флейкает на загруженной машине.
	if flushesDuringClose > 20 {
		t.Fatalf("closeDrain сделал %d флашей за время Close(ctx) с дедлайном 200ms, want <= 20 — дренировал буфер целиком вместо того, чтобы уйти по дедлайну", flushesDuringClose)
	}
}
