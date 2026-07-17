package metric

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

// fakeCHConn/fakeCHBatch повторяют профильные/трейсовые фейки: Append копит строки,
// Send при успехе переносит их в c.rows, а при заданном poison-предикате падает,
// если в батче есть ряд ядовитого name (args[1] в insert — это Name).
type fakeCHConn struct {
	mu     sync.Mutex
	rows   int
	sends  int
	fail   bool // если true — Send падает транзиентной (не серверной) ошибкой
	poison func(name string) bool
}

type fakeCHBatch struct {
	conn    *fakeCHConn
	pending int
	names   []string
}

func (b *fakeCHBatch) Append(args ...any) error {
	b.pending++
	if len(args) > 1 {
		if n, ok := args[1].(string); ok {
			b.names = append(b.names, n)
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
		for _, n := range b.names {
			if b.conn.poison(n) {
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

func TestMetricWriterIsolatesPoisonRowAfterThreshold(t *testing.T) {
	// conn.Send падает, если среди рядов есть метрика с Name=="poison".
	c := &fakeCHConn{poison: func(name string) bool { return name == "poison" }}
	w := NewWriter(c)
	now := time.Now().UTC()
	w.Add(1, MetricPoint{Name: "poison", Type: "gauge", TS: now, Value: 1})
	for i := 0; i < 5; i++ {
		w.Add(1, MetricPoint{Name: "ok", Type: "gauge", TS: now, Value: 1})
	}
	// Прогоняем flush больше порога: обычный ретрай застревает на ядовитом ряду,
	// после poisonThreshold подряд-фейлов должна сработать изоляция.
	for i := 0; i < poisonThreshold+1; i++ {
		w.flush(context.Background())
	}
	if got := w.buffered(); got != 0 {
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

// Транзиентный отказ (сеть/ctx): изоляция не должна дропать валидные метрики.
func TestMetricWriterTransientFailureDropsNothing(t *testing.T) {
	c := &fakeCHConn{fail: true}
	w := NewWriter(c)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		w.Add(1, MetricPoint{Name: "ok", Type: "gauge", TS: now, Value: 1})
	}
	for i := 0; i < poisonThreshold+3; i++ {
		w.flush(context.Background())
	}
	if got := w.buffered(); got != 4 {
		t.Fatalf("buffered = %d, want 4 (ряды остаются на ретрай)", got)
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0 (транзиент не должен ничего терять)", got)
	}
}
