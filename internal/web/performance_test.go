package web_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// perfStack — свой стенд, как monitorsStack, но подключает h.Trace
// (trace.Query, чтение агрегатов из ClickHouse) и h.PerfIssues
// (trace.IssueService, связанные проблемы эндпойнта из PG); наполнение CH — через
// trace.SpanWriter.
type perfStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	writer *trace.SpanWriter
}

func newPerfStack(t *testing.T) *perfStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query // страницы производительности его не используют

	writer := trace.NewSpanWriter(ch)
	go writer.Run()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	})

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, issueSvc, events, srv.URL)
	h.Trace = trace.NewQuery(ch)
	h.PerfIssues = trace.NewIssueService(pool)
	h.Register(mux)

	return &perfStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, writer: writer}
}

// flush синхронно выгружает буфер SpanWriter в ClickHouse (как flush в
// monitors_test.go): дальнейшие Add после этого зависли бы до второго Close из
// Cleanup, поэтому тесты вызывают его один раз после засева всех строк.
func (s *perfStack) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.writer.Close(ctx); err != nil {
		t.Fatalf("flush writer: %v", err)
	}
}

// TestWebPerformanceList — owner видит эндпойнты с перцентилями и <svg,
// «no data» пустой проект не роняет, environment фильтрует список, чужой проект
// → 404.
func TestWebPerformanceList(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perflist-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "perflist-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "perflist-co", "PerfList Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "perflist-proj", "PerfList Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	empty, err := s.org.CreateProject(context.Background(), o.ID, "perflist-empty", "PerfList Empty", "go")
	if err != nil {
		t.Fatalf("create empty project: %v", err)
	}

	// «GET /api/users», production: 20 транзакций, длительности растут, первые
	// две — internal_error (failure rate 0.10). Имя с пробелом и слэшем — заодно
	// проверка URL-кодирования в ссылке на детальную страницу.
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := 0; i < 20; i++ {
		status := "ok"
		if i < 2 {
			status = "internal_error"
		}
		at := base.Add(time.Duration(i) * time.Second)
		dur := time.Duration(i+1) * 10 * time.Millisecond
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("plist-users-%02d", i),
			SpanID:      fmt.Sprintf("plist-uspan-%02d", i),
			Name:        "GET /api/users",
			Op:          "http.server",
			Status:      status,
			Start:       at,
			End:         at.Add(dur),
			Environment: "production",
		})
	}
	// «GET /api/health», staging: 5 транзакций — для проверки фильтра окружения.
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("plist-health-%02d", i),
			SpanID:      fmt.Sprintf("plist-hspan-%02d", i),
			Name:        "GET /api/health",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(5 * time.Millisecond),
			Environment: "staging",
		})
	}
	s.flush(t)

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance"

	// Owner: обе транзакции, <svg-спарклайн, apdex/failure — страница
	// отрендерилась.
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	for _, want := range []string{"GET /api/users", "GET /api/health", "<svg", "Apdex", "ms"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", path, want, body)
		}
	}

	// Environment=staging: только staging-эндпойнт, users не показывается.
	resp = getWithCookie(t, s.srv, path+"?environment=staging", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?environment=staging status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "GET /api/health") {
		t.Fatalf("GET %s?environment=staging missing health endpoint: %s", path, body)
	}
	if strings.Contains(string(body), "GET /api/users") {
		t.Fatalf("GET %s?environment=staging must not show production users endpoint: %s", path, body)
	}

	// Пустой проект: «no transaction data yet», не падает.
	emptyPath := "/projects/" + strconv.FormatInt(empty.ID, 10) + "/performance"
	resp = getWithCookie(t, s.srv, emptyPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (empty) status = %d, want 200: %s", emptyPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Пока нет транзакций") {
		t.Fatalf("GET %s (empty) missing 'no transaction data yet': %s", emptyPath, body)
	}

	// Чужой проект → 404.
	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}

// TestWebEndpointDetail — страница эндпойнта показывает самые медленные трейсы со
// ссылками на /traces/{trace_id} и графики (<svg); несуществующий эндпойнт → 200
// с пустыми графиками, без паники; чужой проект → 404.
func TestWebEndpointDetail(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perfdetail-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "perfdetail-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "perfdetail-co", "PerfDetail Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "perfdetail-proj", "PerfDetail Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := 0; i < 6; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		dur := time.Duration(i+1) * 20 * time.Millisecond
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("pdetail-order-%02d", i),
			SpanID:      fmt.Sprintf("pdetail-ospan-%02d", i),
			Name:        "GET /api/orders",
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(dur),
			Environment: "production",
		})
	}

	// Эндпойнт, чьё имя содержит литеральный «%» (высококардинальный класс
	// имён — непараметризованный роут). ServeMux декодирует {transaction...}
	// один раз, поэтому обработчик НЕ должен декодировать повторно: иначе «%20»
	// превратится в пробел и запрос уйдёт за данными другого (несуществующего)
	// эндпойнта.
	const pctName = "GET /api/orders%20special"
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("pdetail-pct-%02d", i),
			SpanID:      fmt.Sprintf("pdetail-pspan-%02d", i),
			Name:        pctName,
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(30 * time.Millisecond),
			Environment: "production",
		})
	}
	s.flush(t)

	txPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" + url.PathEscape("GET /api/orders")

	resp := getWithCookie(t, s.srv, txPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", txPath, resp.StatusCode, body)
	}
	for _, want := range []string{"GET /api/orders", "<svg", "/traces/pdetail-order-05"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (owner) missing %q: %s", txPath, want, body)
		}
	}

	// Эндпойнт с «%» в имени: детальная страница должна показать ЕГО данные
	// (ссылку на его трейс), а не 404/чужой эндпойнт из-за двойного декодирования.
	pctPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" + url.PathEscape(pctName)
	resp = getWithCookie(t, s.srv, pctPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (pct name) status = %d, want 200: %s", pctPath, resp.StatusCode, body)
	}
	for _, want := range []string{pctName, "/traces/pdetail-pct-03"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s (pct name) missing %q — double-decoded transaction name?: %s", pctPath, want, body)
		}
	}

	// Несуществующий эндпойнт: 200, пустые графики, без паники.
	missingPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" + url.PathEscape("GET /api/nope")
	resp = getWithCookie(t, s.srv, missingPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (missing endpoint) status = %d, want 200: %s", missingPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "GET /api/nope") {
		t.Fatalf("GET %s (missing endpoint) missing title: %s", missingPath, body)
	}

	// Чужой проект → 404.
	resp = getWithCookie(t, s.srv, txPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", txPath, resp.StatusCode)
	}
}

// TestWebPerformanceListTruncates — при большом числе эндпойнтов (непараметри-
// зованные роуты) список усекается до top-N: рендерится не больше N строк и
// показывается пометка об усечении. Иначе на каждую строку идёт отдельный
// CH-запрос спарклайна — тысячи последовательных round-trip'ов на загрузку.
func TestWebPerformanceListTruncates(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perftrunc-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perftrunc-co", "PerfTrunc Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "perftrunc-proj", "PerfTrunc Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	const total = 120 // > лимита списка (100)
	base := time.Now().UTC().Add(-10 * time.Minute)
	for i := 0; i < total; i++ {
		at := base.Add(time.Duration(i) * time.Millisecond)
		s.writer.Add(proj.ID, trace.Transaction{
			TraceID:     fmt.Sprintf("ptrunc-%03d", i),
			SpanID:      fmt.Sprintf("ptruncspan-%03d", i),
			Name:        fmt.Sprintf("GET /api/route/%03d", i),
			Op:          "http.server",
			Status:      "ok",
			Start:       at,
			End:         at.Add(5 * time.Millisecond),
			Environment: "production",
		})
	}
	s.flush(t)

	path := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance"
	resp := getWithCookie(t, s.srv, path, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
	}
	// Каждая строка таблицы даёт одну ссылку вида /projects/N/performance/{tx}.
	rows := strings.Count(string(body), "/performance/")
	if rows > 100 {
		t.Fatalf("rendered %d endpoint rows, want at most 100 (list not truncated)", rows)
	}
	if !strings.Contains(string(body), "Показаны первые") {
		t.Fatalf("missing truncation notice for %d endpoints: %s", total, body)
	}
}

// TestWebIssuesPageHasPerformanceLink — страница issues проекта ссылается на его
// список эндпойнтов (навигация «Performance» рядом с «Monitors»).
func TestWebIssuesPageHasPerformanceLink(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perflink-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perflink-co", "PerfLink Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "perflink-proj", "PerfLink Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/issues"
	perfPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance"

	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), perfPath) {
		t.Fatalf("GET %s missing performance link %q: %s", issuesPath, perfPath, body)
	}
}
