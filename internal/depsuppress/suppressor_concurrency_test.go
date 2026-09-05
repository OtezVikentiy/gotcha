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

// TestGetSnapshotGuardKeepsNewerSnapshot — фикс-раунд ревью на K1-5: guard в
// getSnapshot не должен давать снимку со СТАРШИМ loadedAt затереть уже
// сохранённый в кеше более свежий (TOCTOU-откат кеша назад во времени).
//
// Живой сценарий — конкурентный: горутина A начинает загрузку раньше, но
// вытесняется планировщиком (GC-пауза, нагрузка) между возвратом из
// loadSnapshot и повторным Lock; горутина B стартует позже, успевает
// загрузить и записать более свежий снимок; A просыпается и пытается
// перетереть его своим более старым. Guard проверяет ровно одно значимое
// сравнение — snap.loadedAt против уже сохранённого s.cache.loadedAt — под
// мьютексом, атомарно; какая именно горутина и в каком порядке РЕАЛЬНО
// стартовала, для этого сравнения не имеет значения, важна только пара
// значений loadedAt в момент попытки записи. Это делает воспроизведение
// детерминированным без имитации самого вытеснения: два последовательных
// вызова getSnapshot на одной горутине с управляемым s.now() дают ТУ ЖЕ
// пару значений (сохранённый tNew, затем попытка записи с tOld < tNew) и
// исполняют ровно ту же ветку guard'а, что и живая гонка — без
// синтетических хуков планировщика и без риска гоняющегося по времени
// (flaky) теста.
//
// s.now() дергается дважды за второй вызов: сначала в проверке свежести
// кеша (управляем tFuture, чтобы кеш признали протухшим и пошли за
// перезагрузкой), затем в конце loadSnapshot — там подставляется tOld.
func TestGetSnapshotGuardKeepsNewerSnapshot(t *testing.T) {
	pool := testenv.MigratedPG(t)
	ctx := context.Background()

	tNew := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tFuture := tNew.Add(2 * cacheTTL) // > cacheTTL после tNew — кеш признаётся протухшим
	tOld := tNew.Add(-time.Hour)      // старше уже сохранённого снимка

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

	// Вызов #1: кеш пуст → guard пишет безусловно (нечего защищать), снимок
	// получает loadedAt=tNew.
	if _, err := sup.getSnapshot(ctx); err != nil {
		t.Fatalf("getSnapshot #1: %v", err)
	}
	if sup.cache == nil || !sup.cache.loadedAt.Equal(tNew) {
		t.Fatalf("после #1 cache.loadedAt = %v, want %v", sup.cache, tNew)
	}

	// Вызов #2: свежесть кеша меряется по tFuture → кеш протух → идёт
	// перезагрузка, но сам загруженный снимок получает loadedAt=tOld —
	// СТАРШЕ уже сохранённого tNew. Без guard'а (безусловная запись
	// s.cache = snap) это перетёрло бы cache.loadedAt на tOld.
	if _, err := sup.getSnapshot(ctx); err != nil {
		t.Fatalf("getSnapshot #2: %v", err)
	}
	if !sup.cache.loadedAt.Equal(tNew) {
		t.Fatalf("cache.loadedAt = %v после #2, want %v (guard обязан отклонить более старый снимок tOld=%v)",
			sup.cache.loadedAt, tNew, tOld)
	}
}
