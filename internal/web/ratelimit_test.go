package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterSweepExpiredEntries(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	rl := newRateLimiter(clock, 5, 10*time.Second, 20000, "test")

	// Insert 10001 distinct keys to trigger sweep
	for i := 0; i < 10001; i++ {
		key := key(i)
		rl.Allow(key)
	}

	initialSize := rl.size()
	if initialSize != 10001 {
		t.Errorf("expected 10001 entries after insertion, got %d", initialSize)
	}

	// Advance clock past the window
	now = now.Add(15 * time.Second)

	// Trigger one more Allow() call to invoke the sweep
	rl.Allow("trigger_sweep")

	finalSize := rl.size()
	if finalSize >= 100 {
		t.Errorf("expected map size to drop significantly after sweep, got %d (should be < 100)", finalSize)
	}

	// Verify that the newly added entry is still there
	if finalSize == 0 {
		t.Errorf("expected at least the newly added 'trigger_sweep' entry, got 0 entries")
	}
}

func key(i int) string {
	// Generate a distinct key for each iteration
	return "192.168.1.1|test" + string(rune(i))
}

// TestSetAgentDistRateLimit — rem-A ops-H4: main.go зовёт этот метод один раз
// при старте с порогом из GOTCHA_DIST_RATE_PER_MIN, перекрывая
// дефолтный лимитер New() (10/мин — рассчитан на одиночный сервер, ломает
// Ansible-раскатку/массовое обновление парка за одним IP).
func TestSetAgentDistRateLimit(t *testing.T) {
	h := New(nil, nil, nil, nil, "http://localhost")
	h.SetAgentDistRateLimit(2)

	req := httptest.NewRequest(http.MethodGet, "/agent/x", nil)
	req.RemoteAddr = "203.0.113.7:1234"
	if !h.agentLimiter.Allow(h.clientIP(req)) {
		t.Fatal("1-й запрос должен пройти под лимитом 2/мин")
	}
	if !h.agentLimiter.Allow(h.clientIP(req)) {
		t.Fatal("2-й запрос должен пройти под лимитом 2/мин")
	}
	if h.agentLimiter.Allow(h.clientIP(req)) {
		t.Fatal("3-й запрос обязан упереться в перекрытый лимит 2/мин")
	}
}

// TestSetAgentDistRateLimitZeroMeansUnlimited — SHOULD из аудита A2: соглашение
// продукта «0 = без границы» (как у *_RETENTION_DAYS) должно работать и здесь.
// До фикса SetAgentDistRateLimit(0) создавал лимитер с limit=0, у которого
// `len(fresh) >= rl.limit` истинно всегда — раздача агента 429-ила бы на любой
// запрос. Теперь 0 (и отрицательные) должны снимать лимит полностью (nil).
func TestSetAgentDistRateLimitZeroMeansUnlimited(t *testing.T) {
	h := New(nil, nil, nil, nil, "http://localhost")
	h.SetAgentDistRateLimit(0)

	if h.agentLimiter != nil {
		t.Fatal("SetAgentDistRateLimit(0) должен обнулять agentLimiter (nil = без лимита), а не создавать лимитер с limit=0")
	}

	req := httptest.NewRequest(http.MethodGet, "/agent/x", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	guarded := h.agentDistRateLimited(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	for i := 0; i < 1000; i++ {
		rec := httptest.NewRecorder()
		guarded(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("запрос %d: status = %d, want 200 — при 0 лимит должен быть снят полностью", i+1, rec.Code)
		}
	}

	// Отрицательное значение — та же гарантия.
	h.SetAgentDistRateLimit(-5)
	if h.agentLimiter != nil {
		t.Fatal("SetAgentDistRateLimit(-5) тоже должен обнулять agentLimiter")
	}

	// Положительное значение — лимитирует как раньше.
	h.SetAgentDistRateLimit(2)
	if h.agentLimiter == nil {
		t.Fatal("SetAgentDistRateLimit(2) должен создавать лимитер")
	}
	if !h.agentLimiter.Allow(h.clientIP(req)) {
		t.Fatal("1-й запрос должен пройти под лимитом 2/мин")
	}
	if !h.agentLimiter.Allow(h.clientIP(req)) {
		t.Fatal("2-й запрос должен пройти под лимитом 2/мин")
	}
	if h.agentLimiter.Allow(h.clientIP(req)) {
		t.Fatal("3-й запрос обязан упереться в лимит 2/мин")
	}
}

// TestPublicRateLimitedGuardsUnauthRoutes фиксирует класс «нет лимита на
// неаутентифицированных роутах»: каждый такой запрос от анонима стоит похода в
// PostgreSQL (резолв heartbeat-токена / токена пробы / слага статус-страницы), а
// пул общий с веб-частью — без капа аноним без единого ключа выбирает пул и
// роняет UI, алерты и квоты.
func TestPublicRateLimitedGuardsUnauthRoutes(t *testing.T) {
	h := &Handler{publicLimiter: newRateLimiter(time.Now, 3, time.Minute, publicLimiterMaxKeys, "publicLimiter")}
	var served int
	guarded := h.publicRateLimited(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})

	var last int
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/status/x", nil)
		req.RemoteAddr = "203.0.113.7:1234"
		guarded(rec, req)
		last = rec.Code
	}
	if served != 3 {
		t.Errorf("обслужено %d запросов, ожидалось 3 (лимит)", served)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("код сверх лимита = %d, want 429", last)
	}

	// Другой IP получает собственный бакет — лимит не глобальный.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status/x", nil)
	req.RemoteAddr = "198.51.100.9:5678"
	guarded(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("другой IP получил %d, want 200: лимит должен быть per-IP", rec.Code)
	}
}
