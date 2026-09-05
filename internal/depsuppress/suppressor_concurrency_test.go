package depsuppress

// Тест K1-5 живёт в package depsuppress (не depsuppress_test) по той же
// причине, что и suppressor_cache_test.go: поле pool у Suppressor
// неэкспортируемое, а именно его подмену на инструментированную обёртку
// требует тест ниже.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

// countingSlowPool оборачивает реальный пул и вставляет фиксированную
// задержку перед каждым Query, считая при этом, сколько вызовов Query
// одновременно "в полёте". Задержка исполняется ДО обращения к реальному
// пулу — соединение из пула не удерживается на время сна, так что тест не
// упирается в лимит соединений пула, только в число горутин.
type countingSlowPool struct {
	real  pgxPool
	delay time.Duration

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
}

func (p *countingSlowPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > p.maxInFlight {
		p.maxInFlight = p.inFlight
	}
	p.mu.Unlock()

	time.Sleep(p.delay)

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()

	return p.real.Query(ctx, sql, args...)
}

func (p *countingSlowPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return p.real.QueryRow(ctx, sql, args...)
}

func (p *countingSlowPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return p.real.Exec(ctx, sql, args...)
}

// TestGetSnapshotDoesNotSerializeOnQuery — K1-5: конкурентные вызовы
// getSnapshot при протухшем кеше не должны сериализоваться на время похода
// в PG. До сужения критической секции мьютекс удерживался на все четыре
// запроса loadSnapshot: тогда единственный запрос "в полёте" в любой момент
// времени был бы ровно один (остальные горутины ждали бы mu.Lock, даже не
// дойдя до Query), и maxInFlight countingSlowPool никогда не поднялся бы
// выше 1. После сужения запросы вынесены из-под мьютекса, и несколько
// горутин с одновременно протухшим кешом идут в PG параллельно — мутация,
// возвращающая lock на весь getSnapshot, валит этот тест обратно на
// maxInFlight <= 1.
func TestGetSnapshotDoesNotSerializeOnQuery(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	slow := &countingSlowPool{real: pool, delay: 60 * time.Millisecond}
	sup := &Suppressor{
		pool:     slow,
		cacheTTL: cacheTTL,
		now:      time.Now,
	}

	const n = 5
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			if _, err := sup.getSnapshot(ctx); err != nil {
				t.Errorf("getSnapshot: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	slow.mu.Lock()
	maxInFlight := slow.maxInFlight
	slow.mu.Unlock()

	if maxInFlight <= 1 {
		t.Fatalf("maxInFlight = %d, want > 1: конкурентные загрузки снимка сериализовались на время запроса", maxInFlight)
	}
}
