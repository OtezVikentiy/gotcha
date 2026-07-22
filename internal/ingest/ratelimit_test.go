package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRateLimiterTokenBucket: бакет отдаёт burst токенов сразу, затем режет, и
// пополняется пропорционально времени.
func TestRateLimiterTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	rl := newRateLimiter(clock, 10, 3) // 10 токенов/с, запас 3
	const key = int64(42)

	// Первые 3 запроса — из накопленного burst.
	for i := 0; i < 3; i++ {
		if !rl.Allow(key) {
			t.Fatalf("request %d denied, want allowed (burst)", i)
		}
	}
	// 4-й — токенов нет.
	if rl.Allow(key) {
		t.Fatal("4th request allowed, want denied (bucket empty)")
	}
	// Через 100 мс при 10/с накопится ровно 1 токен → один запрос проходит.
	now = now.Add(100 * time.Millisecond)
	if !rl.Allow(key) {
		t.Fatal("after refill: denied, want allowed")
	}
	if rl.Allow(key) {
		t.Fatal("after single refill: 2nd allowed, want denied")
	}

	// Другой ключ имеет собственный бакет — не задет соседом.
	if !rl.Allow(int64(43)) {
		t.Fatal("independent key denied, want allowed")
	}
}

// TestRateLimiterDisabled: rate<=0 (или nil-лимитер) пропускает всё.
func TestRateLimiterDisabled(t *testing.T) {
	rl := newRateLimiter(time.Now, 0, 0)
	for i := 0; i < 100; i++ {
		if !rl.Allow(1) {
			t.Fatal("rate<=0 must allow everything")
		}
	}
	var nilRL *rateLimiter
	if !nilRL.Allow(1) {
		t.Fatal("nil limiter must allow everything")
	}
}

// TestHandlerRateLimited: h.rateLimited пишет 429 с Retry-After при исчерпании
// бакета и не трогает ответ, пока токены есть.
func TestHandlerRateLimited(t *testing.T) {
	h := &Handler{rate: newRateLimiter(func() time.Time { return time.Unix(0, 0) }, 1, 1)}

	// Первый запрос проходит (есть 1 токен) — ответ не пишется.
	w1 := httptest.NewRecorder()
	if h.rateLimited(w1, 1, 7) {
		t.Fatal("first call limited, want allowed")
	}
	if w1.Code != http.StatusOK { // recorder default 200, ничего не писали
		t.Fatalf("first call wrote status %d, want untouched", w1.Code)
	}

	// Второй — токенов нет → 429 с Retry-After.
	w2 := httptest.NewRecorder()
	if !h.rateLimited(w2, 1, 7) {
		t.Fatal("second call allowed, want limited")
	}
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w2.Code)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing on rate-limit 429")
	}

	// nil-лимитер (выключен) — никогда не режет.
	hNil := &Handler{}
	if hNil.rateLimited(httptest.NewRecorder(), 1, 7) {
		t.Fatal("nil rate limiter must not limit")
	}
}
