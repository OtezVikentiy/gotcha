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

// versionHandler — GET /version: публичные сведения о сборке (без секретов).
func versionHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(version.Get())
	}
}

// livenessHandler — GET /healthz: жив ли процесс.
//
// Отвечает 200, пока HTTP-сервер обслуживает запросы, и НЕ зависит от
// доступности хранилищ. Раньше зависел, и это делало ручку опасной на
// liveness-пробе: недоступный ClickHouse давал 503, оркестратор перезапускал
// живой контейнер, а каждый перезапуск выбрасывает буферы — то есть ровно ту
// телеметрию, которую буферы и копили, чтобы дождаться возвращения хранилища.
// Сбой хранилища превращался в потерю данных.
//
// Состояние компонентов в теле сохранено: его читают и глазами, и скриптами. Но
// на код ответа оно больше не влияет — за это отвечает /readyz.
func livenessHandler(pg, ch pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := probeComponents(r.Context(), pg, ch)
		status["status"] = "alive"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(status)
	}
}

// readinessHandler — GET /readyz: готов ли инстанс работать.
//
// 503, пока PostgreSQL или ClickHouse недоступны. Это тот ответ, который нужен
// балансировщику, чтобы не слать трафик на реплику, которая всё равно ничего не
// запишет; сюда же смотрит healthcheck контейнера. На первом старте миграции
// держат порт закрытым до минуты, и признака готовности не существовало вовсе.
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

// probeComponents пингует оба хранилища параллельно и возвращает их состояние.
// Детали ошибок (хосты, DSN) уходят только в лог: обе ручки отвечают без
// аутентификации.
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
