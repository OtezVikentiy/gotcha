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

type slowCHConn struct {
	mu    sync.Mutex
	sends int
	delay time.Duration
}

type slowCHBatch struct {
	conn *slowCHConn
	ctx  context.Context
}

func (b *slowCHBatch) Append(args ...any) error      { return nil }
func (b *slowCHBatch) AppendStruct(any) error        { return nil }
func (b *slowCHBatch) Abort() error                  { return nil }
func (b *slowCHBatch) Flush() error                  { return nil }
func (b *slowCHBatch) IsSent() bool                  { return false }
func (b *slowCHBatch) Rows() int                     { return 1 }
func (b *slowCHBatch) Close() error                  { return nil }
func (b *slowCHBatch) Column(int) driver.BatchColumn { return nil }
func (b *slowCHBatch) Columns() []column.Interface   { return nil }

// Как настоящий драйвер: отменённый ctx обрывает вставку. db.BatchContext такой ctx не даёт —
// эта ветка в проде недостижима, но фейк обязан вести себя как conn_batch.go Send().
func (b *slowCHBatch) Send() error {
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case <-time.After(b.conn.delay):
		return nil
	}
}

func (c *slowCHConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sends++
	c.mu.Unlock()
	return &slowCHBatch{conn: c, ctx: ctx}, nil
}

// closeDrain обязан уйти по ctx.Err(), не дренировать буфер целиком: проверку ctx.Done() между
// флашами держит верхний select цикла, не только «флаш не продвинулся».
func TestCloseHonoursDeadlineDespiteSlowFlushes(t *testing.T) {
	c := &slowCHConn{delay: 100 * time.Millisecond}
	w := NewResultWriter(c)
	w.batchSize = 10
	w.interval = time.Hour
	go w.Run()
	for i := 0; i < 500; i++ {
		w.Add(1, 1, "local", time.Now(), Result{OK: true})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	c.mu.Lock()
	sendsBefore := c.sends
	c.mu.Unlock()

	start := time.Now()
	err := w.Close(ctx)
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
