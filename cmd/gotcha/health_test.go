package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

type fakePinger struct {
	err   error
	delay time.Duration
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

func TestHealthzOK(t *testing.T) {
	h := livenessHandler(fakePinger{}, fakePinger{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"postgres":"ok"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHealthzStaysAliveWhenStorageIsDown(t *testing.T) {
	h := livenessHandler(fakePinger{err: errors.New("dial tcp 10.0.0.5:5432: refused")},
		fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: недоступное хранилище не делает процесс мёртвым", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"clickhouse":"unavailable"`) {
		t.Errorf("состояние компонентов пропало из тела: %s", body)
	}
	if strings.Contains(body, "10.0.0.5") {
		t.Errorf("internal error details leaked to body: %s", body)
	}
}

func TestReadyzClickHouseDown(t *testing.T) {
	h := readinessHandler(fakePinger{}, fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"clickhouse":"unavailable"`) {
		t.Errorf("want sanitized status, body = %s", body)
	}
	if strings.Contains(body, "10.0.0.5") {
		t.Errorf("internal error details leaked to body: %s", body)
	}
}

func TestReadyzOK(t *testing.T) {
	h := readinessHandler(fakePinger{}, fakePinger{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHealthcheckRequested(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantOK  bool
		wantURL string
	}{
		{"без аргументов", nil, false, defaultHealthcheckURL},
		{"флаг", []string{"--healthcheck"}, true, defaultHealthcheckURL},
		{"подкоманда", []string{"healthcheck"}, true, defaultHealthcheckURL},
		{"свой url через =", []string{"--healthcheck", "--healthcheck-url=http://127.0.0.1:9999/readyz"}, true, "http://127.0.0.1:9999/readyz"},
		{"свой url отдельным аргументом", []string{"--healthcheck", "--healthcheck-url", "http://127.0.0.1:9999/readyz"}, true, "http://127.0.0.1:9999/readyz"},
		{"обычный запуск", []string{"--mode=web"}, false, defaultHealthcheckURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, ok := healthcheckRequested(tc.args, func(string) string { return "" })
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if url != tc.wantURL {
				t.Errorf("url = %q, want %q", url, tc.wantURL)
			}
		})
	}
}

func TestRunHealthcheckExitCodes(t *testing.T) {
	ready := httptest.NewServer(readinessHandler(fakePinger{}, fakePinger{}))
	defer ready.Close()
	if code := runHealthcheck(ready.URL); code != 0 {
		t.Errorf("готовый инстанс: код выхода %d, want 0", code)
	}

	notReady := httptest.NewServer(readinessHandler(fakePinger{}, fakePinger{err: errors.New("refused")}))
	defer notReady.Close()
	if code := runHealthcheck(notReady.URL); code == 0 {
		t.Errorf("неготовый инстанс: код выхода 0 — контейнер останется healthy при недоступном хранилище")
	}

	if code := runHealthcheck("http://127.0.0.1:1/readyz"); code == 0 {
		t.Errorf("недоступный порт: код выхода 0 — зависший процесс останется healthy")
	}
}

func TestHealthzSlowPostgresDoesNotStarveClickHouse(t *testing.T) {
	// PG (3с) и CH (1.5с) таймаутов: последовательно ~3.5с, параллельно ~2с
	h := readinessHandler(fakePinger{delay: 3 * time.Second}, fakePinger{delay: 1500 * time.Millisecond})
	rec := httptest.NewRecorder()
	start := time.Now()
	h(rec, httptest.NewRequest("GET", "/healthz", nil))
	// порог 3200мс лежит между ~2с (таймаут PG, параллельно) и ~3.5с (пинги последовательно)
	// запас держит от миганий под nice, но ловит регресс на потерю параллелизма
	if elapsed := time.Since(start); elapsed > 3200*time.Millisecond {
		t.Fatalf("handler took %v, pings are not concurrent", elapsed)
	}
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (pg down)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"clickhouse":"ok"`) || !strings.Contains(body, `"postgres":"unavailable"`) {
		t.Errorf("body = %s", body)
	}
}

func TestVersionHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	versionHandler()(rec, httptest.NewRequest(http.MethodGet, "/version", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d, ждали 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var info version.Info
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("невалидный JSON: %v", err)
	}
	if info.Version != version.Version() {
		t.Fatalf("version = %q, ждали %q", info.Version, version.Version())
	}
	if info.Stamped != version.Stamped() {
		t.Fatalf("stamped = %v, ждали %v", info.Stamped, version.Stamped())
	}
	if !strings.Contains(rec.Body.String(), `"stamped"`) {
		t.Fatalf("в JSON /version нет поля stamped: %s", rec.Body.String())
	}
}

func TestHealthzCarriesVersion(t *testing.T) {
	h := livenessHandler(fakePinger{}, fakePinger{})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"] != version.Version() {
		t.Fatalf("healthz.version = %q, ждали %q", body["version"], version.Version())
	}
}

// в отличие от тестов выше — бьёт по маршрутам /healthz и /readyz корневого mux,
// а не вызывает хендлеры напрямую: ловит перестановку регистраций в newRootMux
func TestRootMuxLivenessStaysUpWhileReadinessFails(t *testing.T) {
	var metrics selfmetrics.Registry
	mux := newRootMux(rootDeps{
		pg:          fakePinger{},
		ch:          fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")},
		selfMetrics: &metrics,
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz при недоступном ClickHouse: код %d, ждали 200 (живость не зависит от хранилища)", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz при недоступном ClickHouse: код %d, ждали 503 (писать некуда)", rec.Code)
	}
}
