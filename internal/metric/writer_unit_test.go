package metric

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// poison предикат проверяет args[1] в Append — это Name.
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
	c := &fakeCHConn{poison: func(name string) bool { return name == "poison" }}
	w := NewWriter(c)
	now := time.Now().UTC()
	w.Add(1, MetricPoint{Name: "poison", Type: "gauge", TS: now, Value: 1})
	for i := 0; i < 5; i++ {
		w.Add(1, MetricPoint{Name: "ok", Type: "gauge", TS: now, Value: 1})
	}
	// Больше порога: ретрай застревает на ядовитом ряду, после poisonThreshold
	// подряд-фейлов должна сработать изоляция.
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

func TestWriterBoundsBufferByBytes(t *testing.T) {
	w := NewWriter(nil)
	w.maxBufBytes = 1 << 20
	w.batchSize = 1 << 30

	big := strings.Repeat("a", 256<<10)
	for i := 0; i < 20; i++ {
		w.Add(1, MetricPoint{Name: "m", Attributes: map[string]string{"k": big}})
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
		want += metricRowBytes(w.buf[i])
	}
	got := w.bufBytes
	w.mu.Unlock()
	if got != want {
		t.Fatalf("bufBytes = %d, фактический вес %d — учёт разъехался", got, want)
	}
}

func TestMetricRowBytesCountsHost(t *testing.T) {
	a := metricRowBytes(metricRow{Name: "m"})
	b := metricRowBytes(metricRow{Name: "m", Host: "web-1"})
	if b-a != 5 {
		t.Fatalf("host weight = %d, want 5", b-a)
	}
}

// map[string]string весит на порядок больше суммы длин строк — без надбавки
// байтовый потолок срабатывал бы кратно позже реального RSS буфера. Ожидание —
// литерал, не через attrEntryOverheadBytes: иначе тест переживёт занижение константы.
func TestMetricRowBytesCountsAttributeOverhead(t *testing.T) {
	attrs := map[string]string{"a": "1", "b": "2", "c": "3"} // 3 записи по 1+1 символьных байта
	without := metricRowBytes(metricRow{Name: "m"})
	with := metricRowBytes(metricRow{Name: "m", Attributes: attrs})
	if got, want := with-without, int64(150); got != want { // 3 × (2 символа + 48 надбавки)
		t.Fatalf("attribute weight = %d, want %d", got, want)
	}
}

// Одиночный всплеск не должен доезжать до ClickHouse тиками по batchSize раз
// в interval — успешный флаш с непустым остатком обязан кикнуть следующий сам.
// Без Run()/тикера: вручную проигрываем то, что сделал бы Run(), читая kick.
func TestWriterDrainsBurstWithoutWaitingForTick(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.batchSize = 100

	now := time.Now().UTC()
	const burst = 2500
	for i := 0; i < burst; i++ {
		w.Add(1, MetricPoint{Name: "m", Type: TypeGauge, TS: now, Value: 1})
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

	if got := w.buffered(); got != 0 {
		t.Fatalf("buffered = %d после %d флашей — не самокикнулся до опустошения", got, flushes)
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

// Порог — литерал, а не выражение через attrEntryOverheadBytes: 20 строк по 10
// атрибутов весят 4900Б при заниженной надбавке (16) и 11300Б при верной (48).
func TestWriterAttributeWeightTriggersByteTrim(t *testing.T) {
	w := NewWriter(nil)
	w.maxBuf = 1 << 30
	w.batchSize = 1 << 30
	w.maxBufBytes = 8000

	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		attrs := make(map[string]string, 10)
		for j := 0; j < 10; j++ {
			attrs[strconv.Itoa(j)] = "v"
		}
		w.Add(1, MetricPoint{Name: "m", TS: now, Attributes: attrs})
	}

	if got := w.Dropped(); got == 0 {
		t.Fatal("надбавка за карту атрибутов не учтена — байтовый потолок не сработал")
	}
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
		w.Add(1, MetricPoint{Name: "m", TS: now, Value: 1})
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
