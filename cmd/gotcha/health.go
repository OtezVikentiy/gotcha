package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

type pinger interface {
	Ping(ctx context.Context) error
}

// healthProbeTTL — не троттл на кастомный SLA, а внутренняя защита от амплификации:
// без него один внешний HTTP-запрос стоит двух походов в базы, а ручки публичные.
const healthProbeTTL = 5 * time.Second

// Мьютекс snapshot держится на время всего замера: параллельные запросы внутри
// окна склеиваются в один поход к базам, а не заводят каждый свой.
type healthProbe struct {
	pg, ch pinger
	ttl    time.Duration
	now    func() time.Time

	mu        sync.Mutex
	cached    map[string]string
	checkedAt time.Time
}

func newHealthProbe(pg, ch pinger) *healthProbe {
	return &healthProbe{pg: pg, ch: ch, ttl: healthProbeTTL, now: time.Now}
}

// Замер идёт на context.Background(): анонимный клиент, оборвавший соединение,
// не должен отменять пробу, которую ждут (или получают из кэша) другие запросы.
func (p *healthProbe) snapshot() map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == nil || p.now().Sub(p.checkedAt) >= p.ttl {
		p.cached = probeComponents(context.Background(), p.pg, p.ch)
		p.checkedAt = p.now()
	}
	out := make(map[string]string, len(p.cached)+1)
	for k, v := range p.cached {
		out[k] = v
	}
	out["checked_at"] = p.checkedAt.Format(time.RFC3339)
	return out
}

func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(version.Get())
	}
}

// код ответа не зависит от готовности хранилищ: иначе сбой ClickHouse рестартовал
// бы контейнер и ронял буферы телеметрии, которые как раз ждут его возврата
func livenessHandler(probe *healthProbe) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := probe.snapshot()
		status["status"] = "alive"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(status)
	}
}

func readinessHandler(probe *healthProbe) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := probe.snapshot()
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
