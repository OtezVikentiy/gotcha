package web

import (
	"container/list"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Отдельный механизм от потолка maxKeys: sweepThreshold экономит полные обходы карты до
// потолка, maxKeys держит жёсткую границу памяти независимо от того, сработала ли уборка.
const sweepThreshold = 10000

type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time

	// order/elems держат порядок вставки ключей в hits, синхронно с ней (Allow добавляет в обе,
	// sweepExpired убирает из обеих) — вытеснение на потолке берёт front за O(1), без обхода hits.
	order *list.List
	elems map[string]*list.Element

	// Жёсткий потолок числа ключей, свой для каждого инстанса: у per-IP публичного лимитера и
	// per-account лимитера логина разная ожидаемая кардинальность.
	maxKeys int

	name string

	// Общий троттлинг для фонового и принудительного (на упоре в maxKeys) обхода — не чаще раза
	// в window, иначе принудительная уборка гоняла бы полный обход на каждый запрос сверх потолка.
	lastSweep time.Time

	sweepCalls int
}

func newRateLimiter(now func() time.Time, limit int, window time.Duration, maxKeys int, name string) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		now:     now,
		maxKeys: maxKeys,
		name:    name,
		hits:    make(map[string][]time.Time),
		order:   list.New(),
		elems:   make(map[string]*list.Element),
	}
}

// Уборка устаревших попыток выполняется и при отказе — иначе окно, заполненное давними
// хитами, никогда бы не сдвинулось для заблокированного ключа.
func (rl *rateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := rl.now()

	// Число ключей ограничено потолком maxKeys и периодической уборкой (см. sweepExpired);
	// длина ОТДЕЛЬНОГО ключа — забота вызывающего (см. limiterEmailKeyPart для email-ключей).
	if len(rl.hits) > sweepThreshold && now.Sub(rl.lastSweep) >= rl.window {
		rl.sweepExpired(now)
		rl.lastSweep = now
	}

	_, existed := rl.hits[key]
	if !existed && len(rl.hits) >= rl.maxKeys {
		// Троттлинг здесь — тот же lastSweep/window, что и у фонового пути: без него уборка на
		// каждом запросе поверх потолка возвращает O(n) под мьютексом, усиливая атаку, а не гася её.
		if now.Sub(rl.lastSweep) >= rl.window {
			rl.sweepExpired(now)
			rl.lastSweep = now
		}
		if _, existed = rl.hits[key]; !existed && len(rl.hits) >= rl.maxKeys {
			// Вытесняем самый старый ключ и принимаем новый — отказ здесь клал бы логин и
			// статус-страницы всем, у кого ключ ещё не засветился.
			rl.evictOldestLocked()
		}
	}

	cutoff := now.Add(-rl.window)
	fresh := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	if len(fresh) >= rl.limit {
		rl.setLocked(key, fresh, existed)
		return false
	}
	rl.setLocked(key, append(fresh, now), existed)
	return true
}

// Пишет hits и, для впервые увиденного ключа, заводит его в конец очереди вставки.
func (rl *rateLimiter) setLocked(key string, times []time.Time, existed bool) {
	rl.hits[key] = times
	if !existed {
		rl.elems[key] = rl.order.PushBack(key)
	}
}

// Вызывающий обязан держать rl.mu — сама не блокирует. front — самый старый по вставке ключ.
func (rl *rateLimiter) evictOldestLocked() {
	front := rl.order.Front()
	if front == nil {
		return
	}
	key := front.Value.(string)
	rl.order.Remove(front)
	delete(rl.elems, key)
	delete(rl.hits, key)
}

// Вызывается с удержанным rl.mu — сама не блокирует.
func (rl *rateLimiter) sweepExpired(now time.Time) {
	rl.sweepCalls++
	cutoff := now.Add(-rl.window)
	for key, times := range rl.hits {
		fresh := times[:0]
		for _, t := range times {
			if t.After(cutoff) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) == 0 {
			delete(rl.hits, key)
			if elem, ok := rl.elems[key]; ok {
				rl.order.Remove(elem)
				delete(rl.elems, key)
			}
		} else {
			rl.hits[key] = fresh
		}
	}
}

func (rl *rateLimiter) size() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return len(rl.hits)
}

// Без тела — адресаты этих роутов машинные, страница ошибки им не нужна.
func (h *Handler) publicRateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.publicLimiter != nil && !h.publicLimiter.Allow(h.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// perMinute <= 0 обнуляет лимитер, а не создаёт limit=0: в Allow `len(fresh) >= rl.limit`
// истинно уже при limit=0 — такой лимитер резал бы 429 абсолютно все запросы.
func (h *Handler) SetAgentDistRateLimit(perMinute int) {
	if perMinute <= 0 {
		h.agentLimiter = nil
		return
	}
	h.agentLimiter = newRateLimiter(time.Now, perMinute, time.Minute, agentLimiterMaxKeys, "agentLimiter")
}

// Отдельный лимитер от publicLimiter: вес бинаря агента (~9.3 МиБ) иначе годился бы для DoS
// общего пула соединений.
func (h *Handler) agentDistRateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.agentLimiter != nil && !h.agentLimiter.Allow(h.clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// За reverse-proxy это адрес прокси, а не клиента (см. clientIP).
func remoteHost(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return host
}

// XFF доверяем только когда пир — доверенный прокси (TrustedProxies): иначе клиент
// подделывает заголовок и обходит per-IP лимит.
func (h *Handler) clientIP(r *http.Request) string {
	host := remoteHost(r)
	if len(h.TrustedProxies) == 0 {
		return host
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipInNets(peer, h.TrustedProxies) {
		return host // пир не доверенный прокси — XFF не доверяем
	}
	// Идём по XFF справа налево, берём первый адрес не из доверенного набора — правые хопы
	// дописаны нашими прокси, это и есть клиент, ближайший к первому из них.
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

func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// RFC 5321: максимум пути в конверте — 254 байта (локальная часть + @ + домен).
const maxEmailKeyBytes = 254

const oversizedEmailBucket = "oversized"

// Единственное место, где ограничивается длина email-части ключа: email приходит без лимита
// размера из формы, длинный ключ иначе рос бы карту лимитера неограниченно.
func limiterEmailKeyPart(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	if len(email) > maxEmailKeyBytes {
		return oversizedEmailBucket
	}
	return email
}

func (h *Handler) rateLimitKey(r *http.Request, email string) string {
	return h.clientIP(r) + "|" + limiterEmailKeyPart(email)
}
