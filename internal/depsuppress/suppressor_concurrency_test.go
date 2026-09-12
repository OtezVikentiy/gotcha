package depsuppress

// package depsuppress, не _test: поле pool у Suppressor неэкспортируемо.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
)

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

// Гонку воспроизводим без горутин: два вызова getSnapshot с управляемым s.now()
// дают ту же пару loadedAt, что и живое вытеснение планировщиком.
func TestGetSnapshotGuardKeepsNewerSnapshot(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	tNew := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tFuture := tNew.Add(2 * cacheTTL)
	tOld := tNew.Add(-time.Hour)

	calls := []time.Time{tNew, tFuture, tOld}
	idx := 0
	sup := &Suppressor{
		pool:     pool,
		cacheTTL: cacheTTL,
		now: func() time.Time {
			v := calls[idx]
			if idx < len(calls)-1 {
				idx++
			}
			return v
		},
	}

	if _, err := sup.getSnapshot(ctx); err != nil {
		t.Fatalf("getSnapshot #1: %v", err)
	}
	if sup.cache == nil || !sup.cache.loadedAt.Equal(tNew) {
		t.Fatalf("после #1 cache.loadedAt = %v, want %v", sup.cache, tNew)
	}

	if _, err := sup.getSnapshot(ctx); err != nil {
		t.Fatalf("getSnapshot #2: %v", err)
	}
	if !sup.cache.loadedAt.Equal(tNew) {
		t.Fatalf("cache.loadedAt = %v после #2, want %v (guard обязан отклонить более старый снимок tOld=%v)",
			sup.cache.loadedAt, tNew, tOld)
	}
}
