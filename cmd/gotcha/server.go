package main

import (
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// rootDeps — всё, что newRootMux сегодня замыкает в run(): пул PostgreSQL и
// соединение ClickHouse для проб живости/готовности, реестр самотелеметрии
// для /metrics и уже собранные хендлеры приёма/веб-слоя. IngestHandler/
// WebHandler — nil, если соответствующий режим не запущен (например,
// --mode=uptime): тогда на корневом mux остаются только служебные ручки.
//
// pg/ch типизированы тем же интерфейсом pinger, что и livenessHandler/
// readinessHandler (health.go), а не конкретными *pgxpool.Pool/driver.Conn:
// в run() туда по-прежнему приходят настоящие пул и соединение (они пингер
// удовлетворяют структурно), а тест проб — подставной pinger, отвечающий
// ошибкой, без поднятия настоящего ClickHouse.
//
// Поля названы по переменным run(), из которых они приходят, — вынос
// механический, не переписывание.
type rootDeps struct {
	pg            pinger
	ch            pinger
	selfMetrics   *selfmetrics.Registry
	ingestHandler *ingest.Handler
	webHandler    *web.Handler
}

// newRootMux собирает корневой mux ровно так, как раньше собирал его run():
// пробы живости/готовности, версия, самотелеметрия и, если режим их поднял,
// приёмные и веб-роуты.
//
// Вынесено отдельной функцией, чтобы тест заголовков
// (security_headers_test.go) и тест проб (health_test.go) могли собрать
// РЕАЛЬНЫЙ корневой mux вместо собственной копии проводки с заглушками: пока
// сборка жила только внутри run(), тест был вынужден дублировать её сам и
// проверял свой макет, а не то, что действительно слушает порт.
func newRootMux(deps rootDeps) *http.ServeMux {
	mux := http.NewServeMux()
	// Живость и готовность — разные вопросы, поэтому и разные ручки. /healthz
	// на liveness-пробе больше не перезапускает живой процесс из-за упавшего
	// хранилища, а /readyz честно говорит балансировщику и healthcheck'у
	// контейнера, что писать пока некуда.
	mux.HandleFunc("GET /healthz", livenessHandler(deps.pg, deps.ch))
	mux.HandleFunc("GET /readyz", readinessHandler(deps.pg, deps.ch))
	mux.HandleFunc("GET /version", versionHandler())

	// Без аутентификации, как /healthz и /version: тут нет ни ПДн, ни секретов —
	// только счётчики буферов и потерь. И, в отличие от /healthz, ни одного
	// обращения к БД: /metrics обязан отвечать именно тогда, когда БД лежит.
	mux.HandleFunc("GET /metrics", deps.selfMetrics.Handler())

	if deps.ingestHandler != nil {
		deps.ingestHandler.Register(mux)
	}
	if deps.webHandler != nil {
		deps.webHandler.Register(mux)
	}
	return mux
}

// baseSecurityHeaders ставит заголовки, верные для ЛЮБОГО ответа сервера:
// запрет MIME-sniffing и запрет встраивания в iframe.
//
// Полный набор (CSP, Referrer-Policy, HSTS, Cache-Control) остаётся на
// web.securityHeaders: он знает про TLS-режим инстанса и про то, что страницы
// несут ПДн. Здесь — только то, что не зависит ни от чего и должно быть даже
// на /metrics.
func baseSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// newServer собирает http.Server с таймаутами и базовыми заголовками
// безопасности вокруг переданного mux.
//
// Таймауты обязательны: Go по умолчанию их НЕ ставит, а на этом же mux висят
// публичные приёмные эндпойнты (DSN публичен по замыслу). Без них
// Slowloris — медленная посылка заголовков/тела по байту — держит горутину
// и файловый дескриптор на каждое соединение бесконечно, и тысячи таких
// коннектов кладут приём для всех тенантов. MaxBytesReader от этого не
// спасает (тело просто не дочитывается, соединение живёт). ReadHeaderTimeout
// режет slow-header, ReadTimeout — slow-body, WriteTimeout — медленного
// читателя, IdleTimeout закрывает простаивающие keep-alive.
//
// Вынесено отдельной функцией по той же причине, что и newRootMux: тест
// заголовков обязан собирать тот же сервер, что слушает порт в проде, а не
// произвольный http.Handler, обёрнутый вручную в тесте.
func newServer(cfg *Config, mux http.Handler) *http.Server {
	return &http.Server{
		Addr: cfg.Addr,
		// Базовые заголовки — свойство СЕРВЕРА, а не одного поддерева роутов.
		// web.securityHeaders оборачивает только «/», а приём, /healthz,
		// /readyz, /version и /metrics регистрируются на корневом mux и по
		// правилам Go 1.22 перекрывают его — то есть отвечали без nosniff, и
		// это при Access-Control-Allow-Origin: * на приёме. Защита держалась на
		// том, что json.Encoder экранирует HTML, то есть на поведении
		// библиотеки, а не на заголовке.
		Handler:           baseSecurityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB заголовков — с запасом, но не безлимит
	}
}
