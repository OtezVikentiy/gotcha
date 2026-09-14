package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProfileDecodeBudgetAdmitsWithinLimit(t *testing.T) {
	b := newProfileDecodeBudget()
	b.setLimit(800)

	var inFlightSum, maxObserved int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	const calls = 24
	const weight = 100 // 8 одновременных влезают (800/100), 24 вызова — заведомая перегрузка

	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if !b.acquire(context.Background(), weight) {
				t.Errorf("acquire отказал при достаточном общем бюджете")
				return
			}
			n := atomic.AddInt64(&inFlightSum, weight)
			for {
				old := atomic.LoadInt64(&maxObserved)
				if n <= old || atomic.CompareAndSwapInt64(&maxObserved, old, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			atomic.AddInt64(&inFlightSum, -weight)
			b.release(weight)
		}()
	}
	close(start)
	wg.Wait()

	if maxObserved > 800 {
		t.Fatalf("суммарный вес в работе доходил до %d, бюджет 800 — граница не держится", maxObserved)
	}
	if maxObserved < 700 { // с запасом ниже 800, чтобы не требовать точного совпадения таймингов
		t.Fatalf("суммарный вес в работе не превышал %d при бюджете 800 и %d параллельных вызовах — тест не нагружает бюджет всерьёз",
			maxObserved, calls)
	}
}

func TestProfileDecodeBudgetRejectsOversizedRequestImmediately(t *testing.T) {
	b := newProfileDecodeBudget()
	b.setLimit(100)

	done := make(chan bool, 1)
	// context.Background() никогда не отменяется — возврат с ним доказывает,
	// что ожидания не было (иначе done не получил бы значения вовсе).
	go func() { done <- b.acquire(context.Background(), 101) }()

	select {
	case got := <-done:
		if got {
			t.Fatal("вес больше всего бюджета — не должен допускаться никогда")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire не вернулся за 5s без единого шанса на отмену — значит, ждал вместо мгновенного отказа")
	}
}

func TestProfileDecodeBudgetUnlimitedWithoutSetLimit(t *testing.T) {
	b := newProfileDecodeBudget()
	if !b.acquire(context.Background(), 1<<40) {
		t.Fatal("без setLimit бюджет должен быть неограничен")
	}
	b.release(1 << 40) // не должно паниковать при total<=0
}

func TestProfileDecodeBudgetRespectsContextCancellation(t *testing.T) {
	b := newProfileDecodeBudget()
	b.setLimit(100)
	if !b.acquire(context.Background(), 100) {
		t.Fatal("первый acquire должен пройти — бюджет пуст")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- b.acquire(ctx, 1) }()

	// Дать второму acquire войти в цикл ожидания перед явной отменой — сама
	// отмена, не совпадение с окном таймаута, обязана разбудить его.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case got := <-done:
		if got {
			t.Fatal("бюджет исчерпан целиком (release не вызывался) — acquire не должен допускать")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire не вернулся за 5s после отмены контекста — не заметил ctx.Done()")
	}
}

func TestAcquireProfileDecodeWeighsByRawBytes(t *testing.T) {
	h := NewHandler(nil, nil, nil, 1<<20)
	h.SetProfileDecodeBudgetBytes(1000)

	// 20 байт × 35 = 700 — влезает.
	release, ok := h.acquireProfileDecode(context.Background(), 20)
	if !ok {
		t.Fatal("20 байт при бюджете 1000 обязаны допускаться")
	}
	// Ещё 20 байт (ещё 700) уже не влезут одновременно с первым (700+700>1000).
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, ok := h.acquireProfileDecode(ctx, 20); ok {
		t.Fatal("второй разбор той же оценки не должен был поместиться одновременно с первым")
	}
	release()

	// После release второй проход должен пройти.
	release2, ok := h.acquireProfileDecode(context.Background(), 20)
	if !ok {
		t.Fatal("после release второй разбор обязан пройти")
	}
	release2()
}
