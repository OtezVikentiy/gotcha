package ingest_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/klauspost/compress/zstd"

	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/ingest"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/metric"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
)

type stack struct {
	pool     *pgxpool.Pool
	ch       driver.Conn
	srv      *httptest.Server
	pipeline *ingest.Pipeline
	batcher  *event.Batcher
	spans    *trace.SpanWriter
	orgSvc   *org.Service
	org      org.Org
	project  org.Project
	key      org.Key
	metrics  *fakeMetricSink
	profiles *fakeProfileSink
}

// fakeProfileSink копит принятые профили для проверок envelope/pprof-путей.
type fakeProfileSink struct {
	mu   sync.Mutex
	pros []profile.Profile
}

func (f *fakeProfileSink) Add(_ int64, p profile.Profile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pros = append(f.pros, p)
}

func (f *fakeProfileSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pros)
}

// fakeMetricSink копит принятые metric-точки для проверок эндпоинта /v1/metrics.
type fakeMetricSink struct {
	mu     sync.Mutex
	points []metric.MetricPoint
}

func (f *fakeMetricSink) Add(_ int64, p metric.MetricPoint) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.points = append(f.points, p)
}

func (f *fakeMetricSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.points)
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)
	ctx := context.Background()

	orgSvc := org.NewService(pool, 1_000_000)
	var uid int64
	if err := pool.QueryRow(ctx,
		"INSERT INTO users (email, password_hash) VALUES ('ing@example.com','x') RETURNING id").Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	o, err := orgSvc.CreateOrg(ctx, "ing", "Ing", uid)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	p, err := orgSvc.CreateProject(ctx, o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	k, err := orgSvc.CreateKey(ctx, p.ID)
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	batcher := event.NewBatcher(ch)
	go batcher.Run()
	spans := trace.NewSpanWriter(ch)
	go spans.Run()
	projects := ingest.NewProjectCache(orgSvc)
	pipeline := ingest.NewPipeline(issue.NewService(pool), batcher)
	pipeline.Spans = spans
	pipeline.Perf = trace.NewIssueService(pool)
	pipeline.Projects = projects
	pipeline.Start()
	h := ingest.NewHandler(ingest.NewKeyCache(orgSvc), ingest.NewOrgQuota(orgSvc), pipeline, 1<<20)
	h.TxQuota = ingest.NewOrgTransactionQuota(orgSvc)
	h.Projects = projects
	metrics := &fakeMetricSink{}
	h.Metrics = metrics
	h.MetricQuota = ingest.NewOrgMetricQuota(orgSvc)
	profiles := &fakeProfileSink{}
	h.Profiles = profiles
	h.ProfileQuota = ingest.NewOrgProfileQuota(orgSvc)
	h.DropCounter = orgSvc
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		pipeline.Close()
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = batcher.Close(cctx)
		_ = spans.Close(cctx)
	})
	return &stack{
		pool: pool, ch: ch, srv: srv,
		pipeline: pipeline, batcher: batcher, spans: spans,
		orgSvc: orgSvc, org: o, project: p, key: k, metrics: metrics, profiles: profiles,
	}
}

const testEventJSON = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","level":"error",
"exception":{"values":[{"type":"ValueError","value":"boom 17",
"stacktrace":{"frames":[{"function":"do","module":"app.main","in_app":true}]}}]}}`

func envelopeBody(eventJSON string) string {
	return "{}\n{\"type\":\"event\"}\n" + strings.ReplaceAll(eventJSON, "\n", "") + "\n"
}

func (s *stack) post(t *testing.T, path, body string, gzipped bool, key string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if gzipped {
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		_ = zw.Close()
	} else {
		buf.WriteString(body)
	}
	req, err := http.NewRequest("POST", s.srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if key != "" {
		req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_key="+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func waitIssue(t *testing.T, pool *pgxpool.Pool, projectID int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = pool.QueryRow(context.Background(),
			"SELECT count(*) FROM issues WHERE project_id=$1", projectID).Scan(&n)
		if n == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("issue count != %d after 10s", want)
}

func TestEnvelopeEndToEnd(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	resp := s.post(t, path, envelopeBody(testEventJSON), true, s.key.PublicKey)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitIssue(t, s.pool, s.project.ID, 1)

	// Повтор того же события — та же группа, times_seen=2.
	resp = s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var seen int64
		_ = s.pool.QueryRow(context.Background(),
			"SELECT times_seen FROM issues WHERE project_id=$1", s.project.ID).Scan(&seen)
		if seen == 2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("times_seen != 2 after 10s")
}

func TestStoreEndpoint(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/store/", s.project.ID)
	resp := s.post(t, path, testEventJSON, false, s.key.PublicKey)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitIssue(t, s.pool, s.project.ID, 1)
}

func TestAuthFailures(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	if resp := s.post(t, path, envelopeBody(testEventJSON), false, ""); resp.StatusCode != 401 {
		t.Errorf("no key: %d, want 401", resp.StatusCode)
	}
	if resp := s.post(t, path, envelopeBody(testEventJSON), false, "00000000000000000000000000000000"); resp.StatusCode != 403 {
		t.Errorf("unknown key: %d, want 403", resp.StatusCode)
	}
	// Ключ валиден, но не от этого проекта.
	other := fmt.Sprintf("/api/%d/envelope/", s.project.ID+999)
	if resp := s.post(t, other, envelopeBody(testEventJSON), false, s.key.PublicKey); resp.StatusCode != 403 {
		t.Errorf("project mismatch: %d, want 403", resp.StatusCode)
	}
}

func TestMalformedAndTooLarge(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	if resp := s.post(t, path, "garbage not envelope", false, s.key.PublicKey); resp.StatusCode != 400 {
		t.Errorf("garbage: %d, want 400", resp.StatusCode)
	}
	big := envelopeBody(`{"message":"` + strings.Repeat("a", 2<<20) + `"}`)
	if resp := s.post(t, path, big, false, s.key.PublicKey); resp.StatusCode != 413 {
		t.Errorf("oversized: %d, want 413", resp.StatusCode)
	}
}

// TestZstdEnvelope зеркалит gzip-путь TestEnvelopeEndToEnd, но с
// Content-Encoding: zstd — покрывает zstd-ветку body() целиком, включая
// закрытие декодера.
func TestZstdEnvelope(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := zw.Write([]byte(envelopeBody(testEventJSON))); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	req, err := http.NewRequest("POST", s.srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "zstd")
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_key="+s.key.PublicKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	waitIssue(t, s.pool, s.project.ID, 1)
}

// syncBuf — буфер логов, безопасный при параллельной записи: пока тест держит
// его дефолтным slog-хендлером, в него пишут и фоновые горутины (батчер,
// писатель спанов).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestMixedEnvelopePartialQuotaDropIsLogged: в смешанном envelope'е квота
// исчерпана по ОДНОМУ классу (ошибки), по второму (транзакции) — нет. Ответ 200
// (транзакции приняты), но выброшенные ошибки обязаны быть видны в логе: иначе
// оператор не отличит «ошибок не присылали» от «ошибки молча выброшены».
func TestMixedEnvelopePartialQuotaDropIsLogged(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()
	if err := s.orgSvc.SetQuota(ctx, s.org.ID, 1); err != nil {
		t.Fatalf("SetQuota: %v", err)
	}
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	// Выбираем квоту ошибок целиком.
	if resp := s.post(t, path, envelopeBody(testEventJSON), false, s.key.PublicKey); resp.StatusCode != 200 {
		t.Fatalf("first event: status = %d, want 200", resp.StatusCode)
	}

	var logs syncBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	mixed := "{}\n" +
		"{\"type\":\"event\"}\n" + strings.ReplaceAll(testEventJSON, "\n", "") + "\n" +
		"{\"type\":\"transaction\"}\n" + strings.ReplaceAll(freshTransactionJSON(), "\n", "") + "\n"
	resp := s.post(t, path, mixed, false, s.key.PublicKey)
	if resp.StatusCode != 200 {
		t.Fatalf("mixed envelope: status = %d, want 200 (transactions are within quota)", resp.StatusCode)
	}

	out := logs.String()
	if !strings.Contains(out, "class=event") {
		t.Errorf("log does not name the dropped class:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("project_id=%d", s.project.ID)) ||
		!strings.Contains(out, fmt.Sprintf("org_id=%d", s.org.ID)) {
		t.Errorf("log does not name project/org:\n%s", out)
	}
}

// TestDecompressedBombIs413: сырое тело маленькое (gzip), но распакованное
// сообщение превышает и maxBytes, и maxBytes*10 — ожидаем явный 413, а не
// тихую обрезку/400.
func TestDecompressedBombIs413(t *testing.T) {
	s := newStack(t)
	path := fmt.Sprintf("/api/%d/envelope/", s.project.ID)

	big := envelopeBody(`{"message":"` + strings.Repeat("a", 15<<20) + `"}`)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(big)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req, err := http.NewRequest("POST", s.srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_key="+s.key.PublicKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}
