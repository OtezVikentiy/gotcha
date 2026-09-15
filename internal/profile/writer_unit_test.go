package profile

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/column"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type fakeCHConn struct {
	mu     sync.Mutex
	rows   int
	sends  int
	fail   bool                          // если true — Send падает транзиентной (не серверной) ошибкой
	badCtx bool                          // BatchContext не пришёл: Done() != nil или Deadline() не взведён
	poison func(profileType string) bool // если задан и в батче есть ядовитый ряд — Send падает
}

type fakeCHBatch struct {
	conn    *fakeCHConn
	pending int
	types   []string // profile_type каждого добавленного ряда (для poison-предиката)
}

func (b *fakeCHBatch) Append(args ...any) error {
	b.pending++
	if len(args) > 1 {
		if t, ok := args[1].(string); ok {
			b.types = append(b.types, t)
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
		for _, t := range b.types {
			if b.conn.poison(t) {
				// Серверная ошибка CH (data-level) — распознаётся как «яд».
				return &clickhouse.Exception{Code: 53, Message: "type mismatch"}
			}
		}
	}
	b.conn.rows += b.pending
	return nil
}

func (c *fakeCHConn) PrepareBatch(ctx context.Context, _ string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	c.mu.Lock()
	c.sends++
	if _, ok := ctx.Deadline(); ctx.Done() != nil || !ok {
		c.badCtx = true
	}
	c.mu.Unlock()
	return &fakeCHBatch{conn: c}, nil
}

// flushDetached обязан заворачивать ctx в db.BatchContext и на тике Run, и на закрытии — Done()
// == nil одного Background() мало (тавтология): проверяем ещё и взведённый Deadline().
func TestFlushDetachedContextNotCancelableViaRun(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.interval = 20 * time.Millisecond
	go w.Run()
	now := time.Now().UTC()
	w.Add(1, Profile{Type: "cpu", Timestamp: now, Samples: []Sample{
		{Stack: []Frame{{Function: "ok"}}, Value: 1},
	}})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		sends := c.sends
		c.mu.Unlock()
		if sends > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = w.Close(context.Background())

	c.mu.Lock()
	sawBadCtx := c.badCtx
	c.mu.Unlock()
	if sawBadCtx {
		t.Fatal("PrepareBatch увидел не-BatchContext через тик Run (Done() != nil либо Deadline() не взведён)")
	}
}

func TestFlushDetachedContextNotCancelableViaClose(t *testing.T) {
	c := &fakeCHConn{}
	w := NewWriter(c)
	w.interval = time.Hour
	go w.Run()
	now := time.Now().UTC()
	w.Add(1, Profile{Type: "cpu", Timestamp: now, Samples: []Sample{
		{Stack: []Frame{{Function: "ok"}}, Value: 1},
	}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c.mu.Lock()
	sawBadCtx := c.badCtx
	c.mu.Unlock()
	if sawBadCtx {
		t.Fatal("PrepareBatch увидел не-BatchContext через Close/closeDrain (Done() != nil либо Deadline() не взведён)")
	}
}

func TestWriterIsolatesPoisonRowAfterThreshold(t *testing.T) {
	c := &fakeCHConn{poison: func(pt string) bool { return pt == "poison" }}
	w := NewWriter(c)
	now := time.Now().UTC()
	w.Add(1, Profile{Type: "poison", Timestamp: now, Samples: []Sample{
		{Stack: []Frame{{Function: "boom"}}, Value: 1},
	}})
	for i := 0; i < 5; i++ {
		w.Add(1, Profile{Type: "cpu", Timestamp: now, Samples: []Sample{
			{Stack: []Frame{{Function: "ok"}}, Value: 1},
		}})
	}
	// обычный ретрай застревает на ядовитом ряду — после poisonThreshold подряд-фейлов
	// должна сработать изоляция.
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

func TestWriterTransientFailureDropsNothing(t *testing.T) {
	c := &fakeCHConn{fail: true}
	w := NewWriter(c)
	now := time.Now().UTC()
	for i := 0; i < 4; i++ {
		w.Add(1, Profile{Type: "cpu", Timestamp: now, Samples: []Sample{
			{Stack: []Frame{{Function: "ok"}}, Value: 1},
		}})
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

	big := strings.Repeat("F", 64<<10)
	for i := 0; i < 40; i++ {
		w.Add(1, Profile{Samples: []Sample{{
			Stack: []Frame{
				{Function: big + strconv.Itoa(i)},
				{Function: big}, {Function: big}, {Function: big},
			},
			Value: 1,
		}}})
	}

	w.mu.Lock()
	rows, bytes, dropped := len(w.buf), w.bufBytes, w.dropped
	var want int64
	for i := range w.buf {
		want += profileRowBytes(w.buf[i])
	}
	w.mu.Unlock()

	if rows >= 40 {
		t.Fatalf("в буфере %d строк из 40 — байтовый потолок не сработал", rows)
	}
	if dropped == 0 {
		t.Fatal("ничего не выброшено, хотя буфер переполнен по байтам")
	}
	if bytes != want {
		t.Fatalf("bufBytes = %d, фактический вес %d — учёт разъехался", bytes, want)
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
		w.Add(1, Profile{Type: "cpu", Timestamp: now, Samples: []Sample{
			{Stack: []Frame{{Function: "f"}}, Value: 1},
		}})
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

func captureWarnLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
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
		w.Add(1, Profile{Type: "cpu", Timestamp: now, Samples: []Sample{
			{Stack: []Frame{{Function: "f"}}, Value: 1},
		}})
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

// Кадр с U+001F в имени не должен давать ключ агрегации, совпадающий с ключом
// стека из двух обычных кадров, склеенных тем же разделителем stackSep.
func TestFrameKeyEscapesStackSeparator(t *testing.T) {
	twoFrames := strings.Join([]string{
		FrameKey(Frame{Function: "f1"}),
		FrameKey(Frame{Function: "f2"}),
	}, stackSep)

	oneFrameWithSepInName := strings.Join([]string{
		FrameKey(Frame{Function: "f1" + stackSep + "f2"}),
	}, stackSep)

	if twoFrames == oneFrameWithSepInName {
		t.Fatalf("ключи двух разных стеков совпали: %q — U+001F в имени кадра не экранирован", twoFrames)
	}
}
