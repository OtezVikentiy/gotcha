package ingest

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingestsignal"
)

// Гейт обязан отсечь запрос сверх бюджета ДО хендлера, не только вернуть 429.
func TestPreAuthGateBlocksOverBudgetByIP(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	h := &Handler{preAuth: newRateLimiter[string](clock, 1, 1)}

	called := 0
	next := h.preAuthGate(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest("POST", "/api/1/store/", nil)
	req.RemoteAddr = "203.0.113.5:5555"

	w1 := httptest.NewRecorder()
	next(w1, req)
	if w1.Code != http.StatusOK || called != 1 {
		t.Fatalf("первый запрос: code=%d called=%d, want 200/1", w1.Code, called)
	}

	w2 := httptest.NewRecorder()
	next(w2, req)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("второй запрос: code=%d, want 429", w2.Code)
	}
	if called != 1 {
		t.Fatalf("next вызван %d раз после исчерпания бюджета, want 1 (до хендлера не должно доходить)", called)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Fatal("Retry-After отсутствует на 429")
	}

	// Независимый IP не задет чужим бюджетом.
	req2 := httptest.NewRequest("POST", "/api/1/store/", nil)
	req2.RemoteAddr = "198.51.100.9:1"
	w3 := httptest.NewRecorder()
	next(w3, req2)
	if w3.Code != http.StatusOK || called != 2 {
		t.Fatalf("независимый IP: code=%d called=%d, want 200/2", w3.Code, called)
	}
}

func TestPreAuthGateNilLimiterAllowsAll(t *testing.T) {
	h := &Handler{}
	called := false
	next := h.preAuthGate(func(w http.ResponseWriter, r *http.Request) { called = true })

	next(httptest.NewRecorder(), httptest.NewRequest("POST", "/api/1/store/", nil))
	if !called {
		t.Fatal("nil preAuth не должен блокировать")
	}
}

func TestPreAuthClientIPFallsBackToRawRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("POST", "/api/1/store/", nil)
	req.RemoteAddr = "not-a-host-port"
	if got := preAuthClientIP(req); got != "not-a-host-port" {
		t.Fatalf("preAuthClientIP = %q, want raw RemoteAddr без порта", got)
	}
}

// Throttle сигналов не должен просачиваться в HTTP-ответ клиенту.
func TestTouchUnverifiedSignalRateLimitsByIPWithoutChangingResponse(t *testing.T) {
	fr := &fakeResolver{}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	sig := &fakeSignalRecorder{}
	h.Signals = sig
	now := time.Unix(0, 0)
	h.SetSignalTouchRateLimit(func() time.Time { return now }, 1, 1)

	call := func(project string) int {
		req := httptest.NewRequest("POST", "/api/"+project+"/envelope/", nil)
		req.SetPathValue("project", project)
		req.RemoteAddr = "203.0.113.9:1"
		rec := httptest.NewRecorder()
		if _, ok := h.authenticate(rec, req, SignalEvent); ok {
			t.Fatalf("project %s: аутентификация неожиданно прошла", project)
		}
		return rec.Code
	}

	if code := call("7"); code != http.StatusUnauthorized {
		t.Fatalf("первый запрос: code=%d, want 401", code)
	}
	// Бюджет исчерпан, но ответ на другой project_id остаётся честным 401.
	if code := call("9"); code != http.StatusUnauthorized {
		t.Fatalf("второй запрос: code=%d, want 401 (throttle сигналов не влияет на ответ)", code)
	}

	want := []recordedTouch{{7, ingestsignal.KindKeyInvalid}}
	if !reflect.DeepEqual(sig.touches, want) {
		t.Errorf("touches = %+v, want %+v (второе касание обязано быть придержано лимитером)", sig.touches, want)
	}
}

func TestTouchUnverifiedSignalNilLimiterAlwaysTouches(t *testing.T) {
	fr := &fakeResolver{}
	h := NewHandler(NewKeyCache(fr), nil, nil, 1<<20)
	sig := &fakeSignalRecorder{}
	h.Signals = sig
	h.signalTouch = nil

	req := httptest.NewRequest("POST", "/api/7/envelope/", nil)
	req.SetPathValue("project", "7")
	h.authenticate(httptest.NewRecorder(), req, SignalEvent)

	want := []recordedTouch{{7, ingestsignal.KindKeyInvalid}}
	if !reflect.DeepEqual(sig.touches, want) {
		t.Errorf("touches = %+v, want %+v", sig.touches, want)
	}
}
