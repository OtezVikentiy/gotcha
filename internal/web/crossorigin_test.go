package web

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureLog подменяет default-логгер slog на буферный текстовый handler —
// тот же приём, что в TestMigrationStagesAreLogged (cmd/gotcha).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func crossOriginPost(h *Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/settings/locale", strings.NewReader("lang=en"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.localeSwitch(w, r)
	return w
}

// TestDenyCrossOriginVisible — отказ same-origin обязан быть видимым: страница
// с объяснением (не голый text/plain "forbidden"), строка в логе и счётчик.
// До этой починки 58 из 60 веток отвечали http.Error без единого следа —
// оператор видел 403 на регистрации при зелёном /readyz и пустом журнале
// (находка №37).
func TestDenyCrossOriginVisible(t *testing.T) {
	buf := captureLog(t)
	h := &Handler{BaseURL: "http://localhost"}

	w := crossOriginPost(h)
	if w.Code != 403 {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html page", ct)
	}
	if body := w.Body.String(); !strings.Contains(body, "GOTCHA_BASE_URL") {
		t.Errorf("body does not mention GOTCHA_BASE_URL:\n%s", body)
	}
	if got := h.CrossOriginRejected(); got != 1 {
		t.Errorf("CrossOriginRejected() = %d, want 1", got)
	}
	logs := buf.String()
	if n := strings.Count(logs, "cross-origin request rejected"); n != 1 {
		t.Fatalf("log lines = %d, want 1:\n%s", n, logs)
	}
	for _, want := range []string{
		"origin=https://evil.example",
		"base_url=http://localhost",
		"method=POST",
		"path=/settings/locale",
		"suppressed=0",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("log line lacks %q:\n%s", want, logs)
		}
	}
}

// TestDenyCrossOriginThrottlesLog — внутри окна троттлинга новые строки не
// пишутся, но и не теряются: их число уходит полем suppressed следующей
// строки, а счётчик растёт на каждый отказ.
func TestDenyCrossOriginThrottlesLog(t *testing.T) {
	buf := captureLog(t)
	h := &Handler{BaseURL: "http://localhost"}

	for i := 0; i < 3; i++ {
		crossOriginPost(h)
	}
	if n := strings.Count(buf.String(), "cross-origin request rejected"); n != 1 {
		t.Fatalf("log lines after 3 rejects = %d, want 1 (throttled):\n%s", n, buf.String())
	}
	if got := h.CrossOriginRejected(); got != 3 {
		t.Errorf("CrossOriginRejected() = %d, want 3", got)
	}

	// Сдвиг «последней строки» за окно: следующий отказ пишет строку и
	// отчитывается за два подавленных.
	h.coThrottle.mu.Lock()
	h.coThrottle.last = time.Now().Add(-coThrottleWindow - time.Second)
	h.coThrottle.mu.Unlock()
	crossOriginPost(h)
	logs := buf.String()
	if n := strings.Count(logs, "cross-origin request rejected"); n != 2 {
		t.Fatalf("log lines after window shift = %d, want 2:\n%s", n, logs)
	}
	if !strings.Contains(logs, "suppressed=2") {
		t.Errorf("second log line lacks suppressed=2:\n%s", logs)
	}
}
