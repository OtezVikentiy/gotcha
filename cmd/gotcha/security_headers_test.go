package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBaseSecurityHeadersCoverServiceRoutes: служебные и приёмные ручки
// регистрируются на корневом mux и по правилам Go 1.22 перекрывают «/», то есть
// проходят мимо web.securityHeaders. Заголовки, верные для любого ответа,
// обязаны стоять на уровне сервера.
func TestBaseSecurityHeadersCoverServiceRoutes(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("gotcha_build_info 1\n"))
	})
	mux.HandleFunc("POST /api/{id}/store/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	})
	h := baseSecurityHeaders(mux)

	for _, tc := range []struct {
		method, path string
	}{
		{"GET", "/healthz"},
		{"GET", "/metrics"},
		{"POST", "/api/7/store/"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options = %q, want nosniff", tc.method, tc.path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s %s: X-Frame-Options = %q, want DENY", tc.method, tc.path, got)
		}
	}
}
