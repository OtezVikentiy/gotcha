package main

import (
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
)

func TestBaseSecurityHeadersCoverServiceRoutes(t *testing.T) {
	// authenticate() возвращает 401 раньше, чем коснётся keys/quota/pipeline,
	// если в запросе нет sentry_key — nil-зависимости здесь безопасны.
	ingestHandler := ingest.NewHandler(nil, nil, nil, 1<<20)
	var metrics selfmetrics.Registry
	srv := newServer(&Config{Addr: ":0"}, newRootMux(rootDeps{
		pg:            fakePinger{},
		ch:            fakePinger{},
		selfMetrics:   &metrics,
		ingestHandler: ingestHandler,
	}))

	for _, tc := range []struct {
		method, path string
	}{
		{"GET", "/healthz"},
		{"GET", "/readyz"},
		{"GET", "/version"},
		{"GET", "/metrics"},
		{"POST", "/api/7/store/"},
	} {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options = %q, want nosniff", tc.method, tc.path, got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s %s: X-Frame-Options = %q, want DENY", tc.method, tc.path, got)
		}
	}
}
