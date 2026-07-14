package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

type pinger interface {
	Ping(ctx context.Context) error
}

func healthHandler(pg, ch pinger) http.HandlerFunc {
	type result struct {
		name string
		err  error
	}
	check := func(ctx context.Context, name string, p pinger, out chan<- result) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out <- result{name, p.Ping(ctx)}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		results := make(chan result, 2)
		go check(r.Context(), "postgres", pg, results)
		go check(r.Context(), "clickhouse", ch, results)

		status := map[string]string{}
		code := http.StatusOK
		for i := 0; i < 2; i++ {
			res := <-results
			if res.err != nil {
				// Детали (хосты, DSN) — только в лог, наружу не отдаём.
				slog.Warn("health check failed", "component", res.name, "error", res.err)
				status[res.name] = "unavailable"
				code = http.StatusServiceUnavailable
			} else {
				status[res.name] = "ok"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(status)
	}
}
