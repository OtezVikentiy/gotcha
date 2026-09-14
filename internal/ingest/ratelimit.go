package ingest

import (
	"math"
	"sync"
	"time"
)

// Щедрые нарочно: срезают явный флуд, не мешая нормальному трафику SDK/коллектора.
const (
	defaultIngestRatePerSec = 500  // устойчивая скорость, запросов/с на проект
	defaultIngestBurst      = 1000 // пиковый запас токенов
)

// Выше defaultIngestRatePerSec: один IP законно обслуживает несколько DSN
// (общий сервер приложений на несколько проектов организации).
const (
	defaultPreAuthRatePerSec = 2000
	defaultPreAuthBurst      = 4000
)

// Много туже preAuth: легитимный несовпадающий DSN бьёт повторно в свой же
// project_id, а не перебирает чужие — редкие касания не должны упираться в лимит.
const (
	defaultSignalTouchRatePerSec = 2
	defaultSignalTouchBurst      = 10
)

const maxRateLimitKeys = 10000

// Дёшев (без похода в БД) — вызывается ДО квоты; K — project_id или IP.
type rateLimiter[K comparable] struct {
	mu      sync.Mutex
	rate    float64 // токенов в секунду
	burst   float64 // максимум накопленных токенов
	now     func() time.Time
	buckets map[K]*rateBucket
}

type rateBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter[K comparable](now func() time.Time, ratePerSec, burst float64) *rateLimiter[K] {
	return &rateLimiter[K]{
		rate:    ratePerSec,
		burst:   burst,
		now:     now,
		buckets: make(map[K]*rateBucket),
	}
}

func (rl *rateLimiter[K]) Allow(key K) bool {
	if rl == nil || rl.rate <= 0 {
		return true
	}
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if len(rl.buckets) >= maxRateLimitKeys {
		rl.evict(now)
	}

	b, ok := rl.buckets[key]
	if !ok {
		b = &rateBucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(rl.burst, b.tokens+elapsed*rl.rate)
			b.last = now
		}
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (rl *rateLimiter[K]) evict(now time.Time) {
	rl.sweep(now)
	if len(rl.buckets) < maxRateLimitKeys {
		return
	}
	half := rl.burst / 2
	for key, b := range rl.buckets {
		if b.tokens >= half {
			delete(rl.buckets, key)
		}
	}
	if len(rl.buckets) < maxRateLimitKeys {
		return
	}
	drop := len(rl.buckets) / 10
	if drop == 0 {
		drop = 1
	}
	for key := range rl.buckets {
		if drop == 0 {
			break
		}
		delete(rl.buckets, key)
		drop--
	}
}

func (rl *rateLimiter[K]) sweep(now time.Time) {
	for key, b := range rl.buckets {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 && math.Min(rl.burst, b.tokens+elapsed*rl.rate) >= rl.burst {
			delete(rl.buckets, key)
		}
	}
}
