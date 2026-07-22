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

// remoteHost — host из RemoteAddr (порт отброшен), т.е. адрес непосредственного
// TCP-пира. За reverse-proxy это адрес прокси, а не клиента (см. clientIP).
func remoteHost(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return host
}

// clientIP — реальный IP клиента, ключ для глобального per-IP лимита (SEC-L2).
//
// По умолчанию (TrustedProxies пуст) — это адрес непосредственного пира
// (RemoteAddr). За TLS-терминирующим reverse-proxy (штатная HTTPS-топология —
// nginx/traefik) RemoteAddr у ВСЕХ клиентов схлопывается в один IP прокси: тогда
// глобальный per-IP лимитер и обесценивается (все — один ключ), и превращается в
// self-DoS (один актор выбирает общий бакет и лочит логин всем). Поэтому, только
// когда непосредственный пир входит в доверенный список TrustedProxies
// (GOTCHA_TRUSTED_PROXIES), доверяем X-Forwarded-For и берём из него настоящего
// клиента. Иначе XFF — данные, подконтрольные клиенту, и их игнорируем (иначе
// тривиальный обход лимита подделкой заголовка).
func (h *Handler) clientIP(r *http.Request) string {
	host := remoteHost(r)
	if len(h.TrustedProxies) == 0 {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipInNets(peer, h.TrustedProxies) {
		return host // пир не доверенный прокси — XFF не доверяем
	}
	// Пир — доверенный прокси: идём по X-Forwarded-For справа налево и берём
	// первый адрес НЕ из доверенного набора — это клиент, ближайший к первому
	// нашему прокси (правые хопы дописаны нашими же прокси).
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		p := strings.TrimSpace(parts[i])
		ip := net.ParseIP(p)
		if ip == nil {
			continue
		}
		if !ipInNets(ip, h.TrustedProxies) {
			return p
		}
	}
	return host // все хопы доверенные или XFF пуст — остаёмся на пире
}

// ipInNets — принадлежит ли ip хоть одной из сетей.
func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// rateLimitKey строит ключ ip|email для per-account лимитера логина/регистрации.
func (h *Handler) rateLimitKey(r *http.Request, email string) string {
	return h.clientIP(r) + "|" + strings.ToLower(strings.TrimSpace(email))
}
