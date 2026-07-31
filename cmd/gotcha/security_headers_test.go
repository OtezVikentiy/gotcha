package main

import (
	"net/http/httptest"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
)

// TestBaseSecurityHeadersCoverServiceRoutes: служебные и приёмные ручки
// регистрируются на корневом mux и по правилам Go 1.22 перекрывают «/», то есть
// проходят мимо web.securityHeaders. Заголовки, верные для любого ответа,
// обязаны стоять на уровне сервера.
//
// Тест собирает РЕАЛЬНЫЙ сервер через newServer(cfg, newRootMux(deps)) — те же
// две функции, что используются в run() (server.go), — и проверяет заголовки
// на настоящих путях: /healthz, /readyz, /version, /metrics и приёмном
// /api/{project}/store/. Раньше тест собирал свой mux с тремя заглушками, и
// замена боевой строки `Handler: baseSecurityHeaders(mux)` на `Handler: mux`
// в newServer никак не красила этот тест — проверялась только миддлварь, а не
// сервер, который реально слушает порт.
func TestBaseSecurityHeadersCoverServiceRoutes(t *testing.T) {
	// authenticate() в ingest.Handler.store возвращает 401 раньше, чем
	// коснётся keys/quota/pipeline, если в запросе нет sentry_key — поэтому
	// nil-зависимости здесь безопасны: заголовки нас интересуют, а не тело
	// ответа приёма.
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
