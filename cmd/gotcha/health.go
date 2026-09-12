package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

type pinger interface {
	Ping(ctx context.Context) error
}

func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(version.Get())
	}
}

// код ответа не зависит от готовности хранилищ: иначе сбой ClickHouse рестартовал
// бы контейнер и ронял буферы телеметрии, которые как раз ждут его возврата
func livenessHandler(pg, ch pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := probeComponents(r.Context(), pg, ch)
		status["status"] = "alive"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(status)
	}
}

func readinessHandler(pg, ch pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := probeComponents(r.Context(), pg, ch)
		code := http.StatusOK
		for _, name := range []string{"postgres", "clickhouse"} {
			if status[name] != "ok" {
				code = http.StatusServiceUnavailable
			}
		}
		status["status"] = "ready"
		if code != http.StatusOK {
			status["status"] = "not ready"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	}
}

// детали ошибок (хосты, DSN) идут только в лог: обе ручки отвечают без аутентификации
func probeComponents(ctx context.Context, pg, ch pinger) map[string]string {
	type result struct {
		name string
		err  error
	}
	check := func(ctx context.Context, name string, p pinger, out chan<- result) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out <- result{name, p.Ping(ctx)}
	}
	results := make(chan result, 2)
	go check(ctx, "postgres", pg, results)
	go check(ctx, "clickhouse", ch, results)

	status := map[string]string{"version": version.Version()}
	for i := 0; i < 2; i++ {
		res := <-results
		if res.err != nil {
			slog.Warn("health check failed", "component", res.name, "error", res.err)
			status[res.name] = "unavailable"
			continue
		}
		status[res.name] = "ok"
	}
	return status
}
