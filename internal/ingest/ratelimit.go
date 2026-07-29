package ingest

import (
	"math"
	"sync"
	"time"
)

// Дефолты per-DSN лимита приёма. Щедрые нарочно: цель — срезать откровенный флуд
// (сотни тысяч запросов/с с одного ключа кладут PG на квота-проверках), а не
// мешать легитимному высокочастотному трафику. Порядок величин выбран так, чтобы
// нормальный SDK/коллектор в них никогда не упирался.
const (
	defaultIngestRatePerSec = 500  // устойчивая скорость, запросов/с на проект
	defaultIngestBurst      = 1000 // пиковый запас токенов
)

// maxRateLimitKeys — верхняя граница числа отслеживаемых проектов. Как у
// KeyCache: поток из миллионов разных project id иначе раздул бы map. При
// переполнении освобождаем место ярусами (см. evict) — никогда полным сбросом.
const maxRateLimitKeys = 10000

// rateLimiter — простой per-key токен-бакет (образец — internal/web/ratelimit.go,
// но там фиксированное окно per-IP; здесь per-DSN и токен-бакет, чтобы приём
// событий держал ровную скорость без резких границ окна). Часы инжектируются для
// тестов без реального sleep. Дёшев: одна блокировка + арифметика на запрос,
// БЕЗ похода в БД — оттого стоит ПЕРЕД quota-проверкой.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64 // токенов в секунду
	burst   float64 // максимум накопленных токенов
	now     func() time.Time
	buckets map[int64]*rateBucket
}

type rateBucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(now func() time.Time, ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{
		rate:    ratePerSec,
		burst:   burst,
		now:     now,
		buckets: make(map[int64]*rateBucket),
	}
}

// Allow берёт один токен из бакета key (project id) и говорит, принимать ли
// запрос. rate<=0 → лимит выключен (всё разрешено). Пустой/новый бакет стартует
// полным (burst), чтобы первый всплеск легитимного трафика не резался.
func (rl *rateLimiter) Allow(key int64) bool {
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
		// Пополнение пропорционально прошедшему времени, но не выше burst.
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

// evict освобождает место в карте бакетов ярусами — от самых безобидных потерь
// к менее безобидным. Вызывается под mu при переполнении.
//
// Раньше здесь стоял ПОЛНЫЙ СБРОС карты, если sweep ничего не освободил. Но
// sweep не освобождает ничего ровно в одном случае: когда все maxRateLimitKeys
// ключей активно расходуют токены, то есть когда лимит и работает. Сброс выдавал
// каждому из них свежий ПОЛНЫЙ бакет — на инстансе с числом активных проектов
// выше потолка карта переполнялась постоянно, и per-DSN лимит приёма
// выключался целиком. Тот же класс дефекта уже чинили в KeyCache (auth.go),
// но исправление тогда получил только он.
func (rl *rateLimiter) evict(now time.Time) {
	// Ярус 1: полностью восстановившиеся — их состояние неотличимо от свежего,
	// потери нулевые.
	rl.sweep(now)
	if len(rl.buckets) < maxRateLimitKeys {
		return
	}
	// Ярус 2: восстановившиеся больше чем наполовину. Такому ключу сброс
	// возвращает меньше половины бакета — цена мала.
	half := rl.burst / 2
	for key, b := range rl.buckets {
		if b.tokens >= half {
			delete(rl.buckets, key)
		}
	}
	if len(rl.buckets) < maxRateLimitKeys {
		return
	}
	// Ярус 3: карта целиком состоит из активно лимитируемых ключей. Вычищаем
	// десятую часть — случайную, порядок обхода map и есть случайность. Это
	// по-прежнему поблажка для тех, кого вычистили, но ограниченная десятой
	// частью вместо всех, и enforcement для остальных сохраняется.
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

// sweep удаляет полностью восстановившиеся (простаивающие) бакеты: их состояние
// неотличимо от свежего. Вызывается под mu при переполнении map.
func (rl *rateLimiter) sweep(now time.Time) {
	for key, b := range rl.buckets {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 && math.Min(rl.burst, b.tokens+elapsed*rl.rate) >= rl.burst {
			delete(rl.buckets, key)
		}
	}
}
