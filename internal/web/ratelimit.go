package web

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// rateLimiter — фиксированное окно: не более limit попыток за window на
// ключ. Часы инжектируются, чтобы юнит-тесты могли перематывать время без
// реальных sleep.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
}

func newRateLimiter(now func() time.Time, limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		now:    now,
		hits:   make(map[string][]time.Time),
	}
}

// Allow регистрирует попытку под ключом key и сообщает, уложилась ли она в
// лимит. Вызовы сверх лимита тоже не забываются (иначе окно никогда не
// сдвинется), но не увеличивают счётчик сверх того, что уже отброшено по
// давности.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()

	// Sweep expired entries if map grows too large
	if len(rl.hits) > 10000 {
		rl.sweepExpired(now)
	}

	cutoff := now.Add(-rl.window)
	fresh := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= rl.limit {
		rl.hits[key] = fresh
		return false
	}
	rl.hits[key] = append(fresh, now)
	return true
}

// sweepExpired removes all entries whose time windows have fully expired.
// Called with lock held, only when map size exceeds threshold.
func (rl *rateLimiter) sweepExpired(now time.Time) {
	cutoff := now.Add(-rl.window)
	for key, times := range rl.hits {
		// Keep only entries within the window
		fresh := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			// Entire window expired, delete the key
			delete(rl.hits, key)
		} else {
			rl.hits[key] = fresh
		}
	}
}

// size returns the current number of keys in the rate limiter map.
// Exported for testing purposes only.
func (rl *rateLimiter) size() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.hits)
}

// extractIP извлекает host из RemoteAddr — ключ для глобального per-IP лимита
// (SEC-L2). Порт отбрасываем, чтобы разные исходящие порты одного клиента
// считались как один IP.
func extractIP(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return host
}

// rateLimitKey строит ключ ip|email для per-account лимитера логина/регистрации.
func rateLimitKey(r *http.Request, email string) string {
	return extractIP(r) + "|" + strings.ToLower(strings.TrimSpace(email))
}
