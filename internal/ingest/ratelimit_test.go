package ingest

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRateLimiterTokenBucket(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	rl := newRateLimiter[int64](clock, 10, 3)
	const key = int64(42)

	for i := 0; i < 3; i++ {
		if !rl.Allow(key) {
			t.Fatalf("request %d denied, want allowed (burst)", i)
		}
	}
	if rl.Allow(key) {
		t.Fatal("4th request allowed, want denied (bucket empty)")
	}
	now = now.Add(100 * time.Millisecond)
	if !rl.Allow(key) {
		t.Fatal("after refill: denied, want allowed")
	}
	if rl.Allow(key) {
		t.Fatal("after single refill: 2nd allowed, want denied")
	}

	if !rl.Allow(int64(43)) {
		t.Fatal("independent key denied, want allowed")
	}
}

func TestRateLimiterDisabled(t *testing.T) {
	rl := newRateLimiter[int64](time.Now, 0, 0)
	for i := 0; i < 100; i++ {
		if !rl.Allow(1) {
			t.Fatal("rate<=0 must allow everything")
		}
	}
	var nilRL *rateLimiter[int64]
	if !nilRL.Allow(1) {
		t.Fatal("nil limiter must allow everything")
	}
}

func TestSetRateLimitZeroDisablesLimit(t *testing.T) {
	h := NewHandler(nil, nil, nil, 0)
	h.SetRateLimit(nil, 0, 0)
	for i := 0; i < 2000; i++ {
		if !h.rate.Allow(42) {
			t.Fatalf("request %d rejected with disabled limit", i)
		}
	}
}

func TestSetPreAuthRateLimitAppliesGivenParams(t *testing.T) {
	h := NewHandler(nil, nil, nil, 0)
	now := time.Unix(0, 0)
	h.SetPreAuthRateLimit(func() time.Time { return now }, 1, 2) // rate=1/с, burst=2

	if !h.preAuth.Allow("203.0.113.1") {
		t.Fatal("1-й запрос отклонён, want allowed (burst)")
	}
	if !h.preAuth.Allow("203.0.113.1") {
		t.Fatal("2-й запрос отклонён, want allowed (burst)")
	}
	if h.preAuth.Allow("203.0.113.1") {
		t.Fatal("3-й запрос допущен, want denied (burst исчерпан)")
	}
}

func TestSetPreAuthRateLimitZeroDisablesLimit(t *testing.T) {
	h := NewHandler(nil, nil, nil, 0)
	h.SetPreAuthRateLimit(nil, 0, 0)
	for i := 0; i < 2000; i++ {
		if !h.preAuth.Allow("203.0.113.1") {
			t.Fatalf("request %d rejected with disabled limit", i)
		}
	}
}

func TestHandlerRateLimited(t *testing.T) {
	h := &Handler{
		rate:     newRateLimiter[int64](func() time.Time { return time.Unix(0, 0) }, 1, 1),
		rejected: newIngestRejectCounters(),
	}
	beforeRejected := h.RejectedBy(RejectRateLimit, SignalEvent)

	w1 := httptest.NewRecorder()
	if h.rateLimited(w1, 1, 7, SignalEvent) {
		t.Fatal("first call limited, want allowed")
	}
	if w1.Code != http.StatusOK {
		t.Fatalf("first call wrote status %d, want untouched", w1.Code)
	}
	if got := h.RejectedBy(RejectRateLimit, SignalEvent); got != beforeRejected {
		t.Fatalf("RejectedBy(rate_limit, event) after allowed call = %d, want %d (не должен расти)", got, beforeRejected)
	}

	w2 := httptest.NewRecorder()
	if !h.rateLimited(w2, 1, 7, SignalEvent) {
		t.Fatal("second call allowed, want limited")
	}
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w2.Code)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After header missing on rate-limit 429")
	}
	if got := h.RejectedBy(RejectRateLimit, SignalEvent); got != beforeRejected+1 {
		t.Fatalf("RejectedBy(rate_limit, event) = %d, want %d", got, beforeRejected+1)
	}

	hNil := &Handler{}
	if hNil.rateLimited(httptest.NewRecorder(), 1, 7, SignalEvent) {
		t.Fatal("nil rate limiter must not limit")
	}
}

func TestRateLimiterOverflowKeepsEnforcement(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	rl := newRateLimiter[int64](clock, 1, 1)

	const filled = maxRateLimitKeys - 1
	for k := int64(0); k < filled; k++ {
		if !rl.Allow(k) {
			t.Fatalf("ключ %d: первый запрос должен пройти", k)
		}
	}
	if len(rl.buckets) != filled {
		t.Fatalf("бакетов %d, want %d", len(rl.buckets), filled)
	}
	if rl.Allow(0) {
		t.Fatal("ключ 0 опустошён, второй запрос должен быть срезан")
	}

	rl.Allow(int64(filled))
	rl.Allow(int64(filled + 1))

	if got := len(rl.buckets); got < maxRateLimitKeys*8/10 {
		t.Fatalf("после переполнения осталось %d бакетов из %d — похоже на полный сброс, "+
			"а он выдаёт свежий полный бакет каждому лимитируемому ключу", got, maxRateLimitKeys)
	}

	denied := 0
	for k := int64(0); k < filled; k++ {
		if _, alive := rl.buckets[k]; alive && !rl.Allow(k) {
			denied++
		}
	}
	if denied < filled*8/10 {
		t.Fatalf("лимит сохранился лишь для %d ключей из %d", denied, filled)
	}
}
