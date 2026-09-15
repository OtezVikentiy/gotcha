package event

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

type fakeConn struct {
	mu     sync.Mutex
	rows   int
	fail   bool
	sends  int
	badCtx bool                        // BatchContext не пришёл: Done() != nil или Deadline() не взведён
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

func (c *fakeConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	c.sends++
	if _, ok := ctx.Deadline(); ctx.Done() != nil || !ok {
		c.badCtx = true
	}
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
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.sends >= 2 })
	c.mu.Lock()
	c.fail = false
	c.mu.Unlock()
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.rows == 2 })
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
	if err := b.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestBatcherIsolatesPoisonRowAfterThreshold(t *testing.T) {
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

func TestBatcherSelfMetrics(t *testing.T) {
	c := &fakeConn{fail: true}
	b := NewBatcher(c)
	b.batchSize = 2
	b.maxBuf = 4
	b.interval = 20 * time.Millisecond
	go b.Run()

	for i := 0; i < 2; i++ {
		b.Add(Event{ID: "x", ProjectID: 1, IssueID: 1, Timestamp: time.Now()})
	}
	// Вставка падает — растут отказы, но данные лежат в буфере, а не потеряны.
	waitFor(t, func() bool { return b.InsertFailures() > 0 })
	if got := b.Buffered(); got == 0 {
		t.Error("Buffered = 0, ожидались строки в буфере при падающей вставке")
	}
	if got := b.Dropped(); got != 0 {
		t.Errorf("Dropped = %d, ожидался 0: неудачная вставка ретраится, это ещё не потеря", got)
	}

	// Переполняем буфер — вот теперь потеря.
	for i := 0; i < 20; i++ {
		b.Add(Event{ID: "y", ProjectID: 1, IssueID: 1, Timestamp: time.Now()})
	}
	waitFor(t, func() bool { return b.Dropped() > 0 })
	if got := b.Buffered(); got > int64(b.maxBuf) {
		t.Errorf("Buffered = %d, больше maxBuf = %d", got, b.maxBuf)
	}

	c.mu.Lock()
	c.fail = false
	c.mu.Unlock()
	_ = b.Close(context.Background())
}

func TestBatcherBoundsBufferByBytes(t *testing.T) {
	b := NewBatcher(nil)
	b.maxBufBytes = 1 << 20 // 1 МиБ, чтобы тест был быстрым
	b.batchSize = 1 << 30   // не даём флашу сработать: conn = nil

	// Каждое событие ~256 КиБ: пяти хватит, чтобы перевалить за мегабайт.
	big := strings.Repeat("x", 256<<10)
	for i := 0; i < 20; i++ {
		b.Add(Event{ID: "e", Stacktrace: big})
	}

	b.mu.Lock()
	rows, bytes, dropped := len(b.buf), b.bufBytes, b.dropped
	b.mu.Unlock()

	if rows >= 20 {
		t.Fatalf("в буфере %d строк из 20 — байтовый потолок не сработал", rows)
	}
	if dropped == 0 {
		t.Fatal("ни одно событие не выброшено, хотя буфер переполнен по байтам")
	}
	// Допускаем перебор на одно событие: последнее добавляется до подрезки, и
	// одну строку trimLocked оставляет всегда.
	if limit := b.maxBufBytes + int64(len(big)) + 64; bytes > limit {
		t.Fatalf("вес буфера %d при потолке %d", bytes, b.maxBufBytes)
	}
}

// После всплеска Batcher обязан слить остаток самокиками, не дожидаясь
// следующего 5с тика: Add() кикает только на переходе через batchSize.
func TestBatcherDrainsBurstWithoutWaitingForTick(t *testing.T) {
	c := &fakeConn{}
	b := NewBatcher(c)
	b.batchSize = 100

	now := time.Now().UTC()
	const burst = 2500
	for i := 0; i < burst; i++ {
		b.Add(Event{ID: "e", Timestamp: now})
	}

	ctx := context.Background()
	flushes := 0
drain:
	for {
		select {
		case <-b.kick:
			b.flush(ctx)
			flushes++
		default:
			break drain
		}
	}

	if got := b.Buffered(); got != 0 {
		t.Fatalf("Buffered = %d после %d флашей — не самокикнулся до опустошения", got, flushes)
	}
	if want := burst / b.batchSize; flushes != want {
		t.Fatalf("флашей = %d, want %d — на всплеск не хватило self-kick'ов", flushes, want)
	}
	c.mu.Lock()
	rows := c.rows
	c.mu.Unlock()
	if rows != burst {
		t.Fatalf("вставлено %d строк, want %d", rows, burst)
	}
}

// flushDetached обязан заворачивать ctx в db.BatchContext и на тике Run, и на закрытии — Done()
// == nil одного Background() мало (тавтология): проверяем ещё и взведённый Deadline().
func TestFlushDetachedContextNotCancelableViaRun(t *testing.T) {
	c := &fakeConn{}
	b := NewBatcher(c)
	b.interval = 20 * time.Millisecond
	go b.Run()
	b.Add(Event{ID: "a"})
	waitFor(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.sends > 0 })
	_ = b.Close(context.Background())

	c.mu.Lock()
	sawBadCtx := c.badCtx
	c.mu.Unlock()
	if sawBadCtx {
		t.Fatal("PrepareBatch увидел не-BatchContext через тик Run (Done() != nil либо Deadline() не взведён)")
	}
}

func TestFlushDetachedContextNotCancelableViaClose(t *testing.T) {
	c := &fakeConn{}
	b := NewBatcher(c)
	b.interval = time.Hour
	go b.Run()
	b.Add(Event{ID: "a"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c.mu.Lock()
	sawBadCtx := c.badCtx
	c.mu.Unlock()
	if sawBadCtx {
		t.Fatal("PrepareBatch увидел не-BatchContext через Close/closeDrain (Done() != nil либо Deadline() не взведён)")
	}
}

func TestBatcherByteAccountingSurvivesDrops(t *testing.T) {
	b := NewBatcher(nil)
	b.maxBuf = 5
	b.batchSize = 1 << 30

	for i := 0; i < 50; i++ {
		b.Add(Event{ID: "e", Message: strings.Repeat("m", i*10)})
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) != b.maxBuf {
		t.Fatalf("строк %d, want %d", len(b.buf), b.maxBuf)
	}
	var want int64
	for i := range b.buf {
		want += eventBytes(b.buf[i])
	}
	if b.bufBytes != want {
		t.Fatalf("bufBytes = %d, а фактический вес буфера %d — учёт разъехался", b.bufBytes, want)
	}
}
