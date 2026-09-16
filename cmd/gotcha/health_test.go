package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/selfmetrics"
	"gitflic.ru/otezvikentiy/gotcha/internal/version"
)

type fakePinger struct {
	err   error
	delay time.Duration
	calls *int32 // не nil — считает вызовы потокобезопасно
}

func (f fakePinger) Ping(ctx context.Context) error {
	if f.calls != nil {
		atomic.AddInt32(f.calls, 1)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

// newTestProbe даёт пробу с управляемыми часами, чтобы двигать TTL без time.Sleep.
func newTestProbe(pg, ch pinger) (*healthProbe, *time.Time) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := newHealthProbe(pg, ch)
	p.now = func() time.Time { return clock }
	return p, &clock
}

// tickingPinger двигает часы пробы прямо во время Ping — так тест может отличить
// «checked_at взят после замера» от «взят до», не прибегая к реальному time.Sleep.
type tickingPinger struct {
	clock *time.Time
	by    time.Duration
}

func (t tickingPinger) Ping(ctx context.Context) error {
	*t.clock = t.clock.Add(t.by)
	return nil
}

func TestHealthzOK(t *testing.T) {
	probe, _ := newTestProbe(fakePinger{}, fakePinger{})
	h := livenessHandler(probe)
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
	probe, _ := newTestProbe(fakePinger{err: errors.New("dial tcp 10.0.0.5:5432: refused")},
		fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")})
	h := livenessHandler(probe)
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
	probe, _ := newTestProbe(fakePinger{}, fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")})
	h := readinessHandler(probe)
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
	probe, _ := newTestProbe(fakePinger{}, fakePinger{})
	h := readinessHandler(probe)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestHealthProbeCachesWithinTTL(t *testing.T) {
	pgCalls, chCalls := new(int32), new(int32)
	probe, _ := newTestProbe(fakePinger{calls: pgCalls}, fakePinger{calls: chCalls})
	h := livenessHandler(probe)

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/healthz", nil))
	}
	if got := atomic.LoadInt32(pgCalls); got != 1 {
		t.Errorf("postgres pings = %d, want 1 (кэш внутри TTL)", got)
	}
	if got := atomic.LoadInt32(chCalls); got != 1 {
		t.Errorf("clickhouse pings = %d, want 1 (кэш внутри TTL)", got)
	}
}

func TestHealthProbeRefreshesAfterTTL(t *testing.T) {
	pgCalls, chCalls := new(int32), new(int32)
	pg := fakePinger{calls: pgCalls}
	probe, clock := newTestProbe(pg, fakePinger{calls: chCalls})
	h := readinessHandler(probe)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if !strings.Contains(rec.Body.String(), `"postgres":"ok"`) {
		t.Fatalf("body до сбоя = %s", rec.Body.String())
	}

	probe.pg = fakePinger{err: errors.New("dial tcp 10.0.0.5:5432: refused"), calls: pgCalls}
	*clock = clock.Add(healthProbeTTL)

	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	if got := atomic.LoadInt32(pgCalls); got != 2 {
		t.Errorf("postgres pings = %d, want 2 (TTL истёк, замер повторён)", got)
	}
	if !strings.Contains(rec.Body.String(), `"postgres":"unavailable"`) {
		t.Errorf("изменившееся состояние базы не доехало в ответ: %s", rec.Body.String())
	}
}

func TestHealthProbeSharedAcrossLivenessAndReadiness(t *testing.T) {
	pgCalls, chCalls := new(int32), new(int32)
	probe, _ := newTestProbe(fakePinger{calls: pgCalls}, fakePinger{calls: chCalls})

	recL := httptest.NewRecorder()
	livenessHandler(probe)(recL, httptest.NewRequest("GET", "/healthz", nil))
	recR := httptest.NewRecorder()
	readinessHandler(probe)(recR, httptest.NewRequest("GET", "/readyz", nil))

	if got := atomic.LoadInt32(pgCalls); got != 1 {
		t.Errorf("postgres pings = %d, want 1 (кэш общий между /healthz и /readyz)", got)
	}
	if got := atomic.LoadInt32(chCalls); got != 1 {
		t.Errorf("clickhouse pings = %d, want 1 (кэш общий между /healthz и /readyz)", got)
	}
}

func TestHealthProbeConcurrentRequestsDoNotMultiplyPings(t *testing.T) {
	pgCalls, chCalls := new(int32), new(int32)
	// небольшая задержка держит окно, в которое должны провалиться все горутины,
	// пока держится мьютекс замера
	probe, _ := newTestProbe(fakePinger{calls: pgCalls, delay: 20 * time.Millisecond},
		fakePinger{calls: chCalls, delay: 20 * time.Millisecond})
	h := readinessHandler(probe)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h(rec, httptest.NewRequest("GET", "/readyz", nil))
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(pgCalls); got != 1 {
		t.Errorf("postgres pings = %d, want 1 (параллельные запросы склеены)", got)
	}
	if got := atomic.LoadInt32(chCalls); got != 1 {
		t.Errorf("clickhouse pings = %d, want 1 (параллельные запросы склеены)", got)
	}
}

// главный инвариант задачи: контекст запроса не должен участвовать в замере.
// Отменённый r.Context() первого запроса не должен портить результат второго.
func TestHealthProbeIgnoresCancelledRequestContext(t *testing.T) {
	pgCalls := new(int32)
	probe, _ := newTestProbe(fakePinger{calls: pgCalls, delay: 20 * time.Millisecond}, fakePinger{})
	h := readinessHandler(probe)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/readyz", nil).WithContext(ctx)
	cancel() // клиент обрывает соединение до того, как замер завершился

	rec := httptest.NewRecorder()
	h(rec, req)
	if strings.Contains(rec.Body.String(), "unavailable") {
		t.Fatalf("отменённый контекст запроса испортил результат замера: %s", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h(rec2, httptest.NewRequest("GET", "/readyz", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("следующий запрос: код %d, want 200 — не должен унаследовать отменённый контекст соседа", rec2.Code)
	}
	if strings.Contains(rec2.Body.String(), "unavailable") {
		t.Errorf("следующий запрос увидел ошибку пинга из-за отменённого контекста соседа: %s", rec2.Body.String())
	}
}

func TestHealthProbeCheckedAt(t *testing.T) {
	probe, clock := newTestProbe(fakePinger{}, fakePinger{})
	h := readinessHandler(probe)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	checkedAt, err := time.Parse(time.RFC3339, body["checked_at"])
	if err != nil {
		t.Fatalf("checked_at не парсится как RFC3339: %q: %v", body["checked_at"], err)
	}
	if !checkedAt.Equal(*clock) {
		t.Errorf("checked_at = %v, want %v", checkedAt, *clock)
	}

	rec2 := httptest.NewRecorder()
	h(rec2, httptest.NewRequest("GET", "/readyz", nil))
	var body2 map[string]string
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatal(err)
	}
	if body2["checked_at"] != body["checked_at"] {
		t.Errorf("checked_at изменился внутри TTL: %q -> %q", body["checked_at"], body2["checked_at"])
	}
}

// закрепляет: checked_at — момент ОКОНЧАНИЯ замера, а не его начала.
func TestHealthProbeCheckedAtIsStampedAfterMeasurement(t *testing.T) {
	probe, clock := newTestProbe(fakePinger{}, fakePinger{})
	start := *clock
	tick := 3 * time.Second
	probe.pg = tickingPinger{clock: clock, by: tick}
	h := readinessHandler(probe)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/readyz", nil))
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	checkedAt, err := time.Parse(time.RFC3339, body["checked_at"])
	if err != nil {
		t.Fatalf("checked_at не парсится как RFC3339: %q: %v", body["checked_at"], err)
	}

	want := start.Add(tick)
	if !checkedAt.Equal(want) {
		t.Errorf("checked_at = %v, want %v (замер сдвинул часы на %v во время пинга, "+
			"checked_at обязан отражать момент ОКОНЧАНИЯ замера, а не начала)", checkedAt, want, tick)
	}
}

func TestReadyzStaysDownAfterOutageWithinTTL(t *testing.T) {
	probe, _ := newTestProbe(fakePinger{}, fakePinger{err: errors.New("dial tcp 10.0.0.5:9000: refused")})
	h := readinessHandler(probe)

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", "/readyz", nil))
		if rec.Code != 503 {
			t.Fatalf("запрос %d: status = %d, want 503", i, rec.Code)
		}
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
	ready := httptest.NewServer(readinessHandler(newHealthProbe(fakePinger{}, fakePinger{})))
	defer ready.Close()
	if code := runHealthcheck(ready.URL); code != 0 {
		t.Errorf("готовый инстанс: код выхода %d, want 0", code)
	}

	notReady := httptest.NewServer(readinessHandler(newHealthProbe(fakePinger{}, fakePinger{err: errors.New("refused")})))
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
	h := readinessHandler(newHealthProbe(fakePinger{delay: 3 * time.Second}, fakePinger{delay: 1500 * time.Millisecond}))
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
	h := livenessHandler(newHealthProbe(fakePinger{}, fakePinger{}))
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
