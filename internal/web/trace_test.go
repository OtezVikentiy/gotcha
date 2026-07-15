package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/profile"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// traceStack — стенд задачи 3: и spans (trace.SpanWriter → waterfall), и
// events (event.Batcher → маркеры ошибок и ссылка «Смотреть трейс» на странице
// issue). Wires h.Trace (waterfall/доступ) и h.Events (ByTraceID, EventByID).
type traceStack struct {
	pool    *pgxpool.Pool
	ch      driver.Conn
	srv     *httptest.Server
	org     *org.Service
	auth    *auth.Service
	issues  *issue.Service
	spans   *trace.SpanWriter
	batcher *event.Batcher
}

func newTraceStack(t *testing.T) *traceStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	eventsQuery := event.NewQuery(ch)

	batcher := event.NewBatcher(ch)
	go batcher.Run()
	spans := trace.NewSpanWriter(ch)
	go spans.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = batcher.Close(ctx)
		_ = spans.Close(ctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, eventsQuery, srv.URL)
	h.Trace = trace.NewQuery(ch)
	h.Profiles = profile.NewQuery(ch)
	h.Register(mux)

	return &traceStack{pool: pool, ch: ch, srv: srv, org: orgSvc, auth: authSvc, issues: issueSvc, spans: spans, batcher: batcher}
}

// flush синхронно выгружает оба буфера (spans и events) в ClickHouse до
// последующих GET. Close идемпотентен — повторный вызов из Cleanup безопасен.
func (s *traceStack) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.spans.Close(ctx); err != nil {
		t.Fatalf("flush spans: %v", err)
	}
	if err := s.batcher.Close(ctx); err != nil {
		t.Fatalf("flush events: %v", err)
	}
}

// TestWebTraceWaterfall — трейс с 3 спанами → waterfall (<svg + 3 <rect); спан
// с привязанной ошибкой → красный маркер со ссылкой на issue; чужой проект →
// 404; несуществующий trace_id → 404.
func TestWebTraceWaterfall(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "trace-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "trace-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "trace-co", "Trace Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "trace-proj", "Trace Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// issue, к которому привяжем ошибку на спане.
	now := time.Now().UTC()
	iss, err := s.issues.Upsert(context.Background(), proj.ID, "fp-trace", "DBError", "GET /api/checkout", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	const traceID = "wf-trace-01"
	const rootSpan = "wf-span-root"
	const dbSpan = "wf-span-db"
	const httpSpan = "wf-span-http"
	start := now.Add(-5 * time.Minute)

	// Транзакция «GET /api/checkout»: корень + 2 дочерних спана (db, http).
	s.spans.Add(proj.ID, trace.Transaction{
		TraceID:     traceID,
		SpanID:      rootSpan,
		Name:        "GET /api/checkout",
		Op:          "http.server",
		Status:      "ok",
		Start:       start,
		End:         start.Add(300 * time.Millisecond),
		Environment: "production",
		Spans: []trace.Span{
			{
				SpanID:       dbSpan,
				ParentSpanID: rootSpan,
				Op:           "db.query",
				Description:  "SELECT * FROM orders",
				Start:        start.Add(20 * time.Millisecond),
				End:          start.Add(120 * time.Millisecond),
				Status:       "internal_error",
			},
			{
				SpanID:       httpSpan,
				ParentSpanID: rootSpan,
				Op:           "http.client",
				Description:  "POST payments",
				Start:        start.Add(150 * time.Millisecond),
				End:          start.Add(280 * time.Millisecond),
				Status:       "ok",
			},
		},
	})

	// Событие-ошибка на dbSpan этого трейса → красный маркер со ссылкой на issue.
	s.batcher.Add(event.Event{
		ID:        uuid.NewString(),
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: start.Add(120 * time.Millisecond),
		Level:     "error",
		Message:   "db boom",
		TraceID:   traceID,
		SpanID:    dbSpan,
		Tags:      map[string]string{},
	})
	s.flush(t)

	tracePath := "/traces/" + traceID

	// Owner: 200, <svg, >= 3 <rect (3 спана), ссылка на issue, имя транзакции.
	resp := getWithCookie(t, s.srv, tracePath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", tracePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("GET %s missing <svg: %s", tracePath, body)
	}
	if n := strings.Count(string(body), "<rect"); n < 3 {
		t.Fatalf("GET %s has %d <rect, want >= 3: %s", tracePath, n, body)
	}
	issueLink := "/issues/" + strconv.FormatInt(iss.IssueID, 10)
	if !strings.Contains(string(body), issueLink) {
		t.Fatalf("GET %s missing error marker link %q: %s", tracePath, issueLink, body)
	}
	if !strings.Contains(string(body), "GET /api/checkout") {
		t.Fatalf("GET %s missing transaction name: %s", tracePath, body)
	}

	// Чужой проект → 404.
	resp = getWithCookie(t, s.srv, tracePath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", tracePath, resp.StatusCode)
	}

	// Несуществующий trace_id → 404.
	resp = getWithCookie(t, s.srv, "/traces/nope-nope", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /traces/nope-nope status = %d, want 404", resp.StatusCode)
	}
}

// TestWebIssueDetailTraceLink — событие с trace_id → на странице issue есть
// ссылка «Смотреть трейс» на /traces/{trace_id}; событие без trace_id → ссылки
// нет.
func TestWebIssueDetailTraceLink(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "tracelink-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "tracelink-co", "TraceLink Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "tracelink-proj", "TraceLink Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	now := time.Now().UTC()
	iss, err := s.issues.Upsert(context.Background(), proj.ID, "fp-tracelink", "Boom", "svc.go:1", "error", "", now)
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	const traceID = "link-trace-01"
	withTraceID := uuid.NewString()
	noTraceID := uuid.NewString()
	s.batcher.Add(event.Event{
		ID:        withTraceID,
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: now.Add(-2 * time.Minute),
		Level:     "error",
		Message:   "with trace",
		TraceID:   traceID,
		Tags:      map[string]string{},
	})
	s.batcher.Add(event.Event{
		ID:        noTraceID,
		ProjectID: proj.ID,
		IssueID:   iss.IssueID,
		Timestamp: now.Add(-1 * time.Minute),
		Level:     "error",
		Message:   "no trace",
		Tags:      map[string]string{},
	})
	s.flush(t)

	issuePath := "/issues/" + strconv.FormatInt(iss.IssueID, 10)

	// Событие с trace_id → ссылка «Смотреть трейс» на /traces/{trace_id}.
	resp := getWithCookie(t, s.srv, issuePath+"?event="+withTraceID, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?event=%s status = %d, want 200: %s", issuePath, withTraceID, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Смотреть трейс") {
		t.Fatalf("GET %s (event with trace) missing 'Смотреть трейс': %s", issuePath, body)
	}
	if !strings.Contains(string(body), "/traces/"+traceID) {
		t.Fatalf("GET %s (event with trace) missing trace link: %s", issuePath, body)
	}

	// Событие без trace_id → ссылки нет.
	resp = getWithCookie(t, s.srv, issuePath+"?event="+noTraceID, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?event=%s status = %d, want 200: %s", issuePath, noTraceID, resp.StatusCode, body)
	}
	if strings.Contains(string(body), "Смотреть трейс") {
		t.Fatalf("GET %s (event without trace) must not show 'Смотреть трейс': %s", issuePath, body)
	}
}

// TestWebTraceProfilingInContext — этап 8: при наличии профиля для трейса
// waterfall показывает ссылку «View flamegraph», а /traces/{id}/flame отдаёт
// flamegraph. Без профиля ссылки нет.
func TestWebTraceProfilingInContext(t *testing.T) {
	s := newTraceStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "pic-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "pic-outsider@example.com")
	ctx := context.Background()
	o, _ := s.org.CreateOrg(ctx, "pic-co", "PIC Co", ownerID)
	proj, _ := s.org.CreateProject(ctx, o.ID, "pic-proj", "PIC Proj", "go")

	const traceID = "pic-trace-01"
	start := time.Now().UTC().Add(-2 * time.Minute)
	s.spans.Add(proj.ID, trace.Transaction{
		TraceID: traceID, SpanID: "pic-root", Name: "GET /pic", Op: "http.server",
		Status: "ok", Start: start, End: start.Add(100 * time.Millisecond), Environment: "prod",
	})
	s.flush(t)

	// Без профиля — кнопки нет.
	resp := getWithCookie(t, s.srv, "/traces/"+traceID, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "View flamegraph") {
		t.Fatalf("flamegraph link shown without a profile")
	}

	// Засеять профиль этого трейса.
	if err := s.ch.Exec(ctx, `INSERT INTO profile_samples
		(project_id,profile_type,service,environment,transaction,platform,ts,stack,value,trace_id)
		VALUES (?,'cpu','api','prod','GET /pic','go',?,?,?,?)`,
		proj.ID, start, []string{"root", "handler"}, uint64(10), traceID); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	// Теперь кнопка есть.
	resp = getWithCookie(t, s.srv, "/traces/"+traceID, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "View flamegraph") {
		t.Fatalf("flamegraph link missing with a profile: %s", body)
	}

	// /traces/{id}/flame отдаёт SVG.
	resp = getWithCookie(t, s.srv, "/traces/"+traceID+"/flame", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "<svg") {
		t.Fatalf("flame page status=%d body=%s", resp.StatusCode, body)
	}

	// Чужой → 404.
	resp = getWithCookie(t, s.srv, "/traces/"+traceID+"/flame", outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider flame status = %d, want 404", resp.StatusCode)
	}
}
