package main

import (
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// pg/ch — интерфейс pinger, не конкретный тип: тест проб подставляет заглушку без ClickHouse.
type rootDeps struct {
	pg            pinger
	ch            pinger
	selfMetrics   *selfmetrics.Registry
	ingestHandler *ingest.Handler
	webHandler    *web.Handler
}

func newRootMux(deps rootDeps) *http.ServeMux {
	mux := http.NewServeMux()
	probe := newHealthProbe(deps.pg, deps.ch)
	mux.HandleFunc("GET /healthz", livenessHandler(probe))
	mux.HandleFunc("GET /readyz", readinessHandler(probe))
	mux.HandleFunc("GET /version", versionHandler())

	// Без авторизации, без обращения к БД — отвечает и когда БД лежит.
	mux.HandleFunc("GET /metrics", deps.selfMetrics.Handler())

	// GOTCHA_COMPOSE_BIND до процесса не доходит — знать, публичен ли хост-порт,
	// он не может, поэтому предупреждение безусловное, на каждый старт.
	slog.Info("/healthz, /readyz, /version and /metrics serve without authentication; " +
		"restrict them to trusted networks with a reverse proxy or firewall (see /docs/hardening)")

	if deps.ingestHandler != nil {
		deps.ingestHandler.Register(mux)
	}
	if deps.webHandler != nil {
		deps.webHandler.Register(mux)
	}
	return mux
}

// Полный набор (CSP, Referrer-Policy, HSTS) — в web.securityHeaders для «/»;
// здесь — общее для всех ответов, включая /metrics.
func baseSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// Таймауты обязательны: Go не ставит их по умолчанию, а без них Slowloris
// держит соединение и дескриптор бесконечно, кладя приём для всех тенантов.
func newServer(cfg *Config, mux http.Handler) *http.Server {
	return &http.Server{
		Addr: cfg.Addr,
		// Обёртка на уровне сервера: /healthz, /readyz, /version, /metrics и
		// приём регистрируются на корневом mux в обход web.securityHeaders «/».
		Handler:           baseSecurityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB заголовков — с запасом, но не безлимит
	}
}
