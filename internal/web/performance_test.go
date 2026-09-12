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

type perfStack struct {
	pool   *pgxpool.Pool
	srv    *httptest.Server
	org    *org.Service
	auth   *auth.Service
	writer *trace.SpanWriter
	// h — нужен тестам, настраивающим h.SpanRetentionDays явно вместо дефолта.
	h *web.Handler
}

func newPerfStack(t *testing.T) *perfStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	var events *event.Query

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

	return &perfStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, writer: writer, h: h}
}

// Дальнейшие Add после Close зависли бы до второго Close из Cleanup — вызывать
// один раз, после засева всех строк.
func (s *perfStack) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.writer.Close(ctx); err != nil {
		t.Fatalf("flush writer: %v", err)
	}
}

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

	// Имя с пробелом и слэшем — заодно проверка URL-кодирования в ссылке на детальную страницу.
	base := time.Now().UTC().Add(-30 * time.Minute)
	for i := 0; i < 20; i++ {
		status := "ok"
		if i < 2 {
			status = "internal_error"
		}
		at := base.Add(time.Duration(i) * time.Second)
		dur := time.Duration(i+1) * 10 * time.Millisecond
		s.writer.Add(proj.ID, proj.ID, trace.Transaction{
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
	for i := 0; i < 5; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, proj.ID, trace.Transaction{
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

	cq := "?period=custom&start=2026-07-01T00:00&end=2026-07-10T00:00"
	resp = getWithCookie(t, s.srv, path+cq, ownerCookie)
	cbody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(cbody), `value="custom" selected`) {
		t.Fatalf("GET %s custom range status=%d: %s", path, resp.StatusCode, cbody)
	}

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

	emptyPath := "/projects/" + strconv.FormatInt(empty.ID, 10) + "/performance"
	resp = getWithCookie(t, s.srv, emptyPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (empty) status = %d, want 200: %s", emptyPath, resp.StatusCode, body)
	}
	// Пустое состояние ведёт с ОКНА ВРЕМЕНИ, а не с настройкой SDK: «нет данных» чаще
	// означает «не в этом окне», а не поломку конфига.
	if !strings.Contains(string(body), "Нет транзакций за период") {
		t.Fatalf("GET %s (empty) missing period-first empty state: %s", emptyPath, body)
	}
	if !strings.Contains(string(body), "расширить окно времени") {
		t.Fatalf("GET %s (empty) не предлагает расширить период: %s", emptyPath, body)
	}

	resp = getWithCookie(t, s.srv, path, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", path, resp.StatusCode)
	}
}

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
		s.writer.Add(proj.ID, proj.ID, trace.Transaction{
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

	// Имя с литеральным «%»: обработчик не должен декодировать его повторно после
	// ServeMux, иначе «%20» превратится в пробел и уйдёт за данными другого эндпойнта.
	const pctName = "GET /api/orders%20special"
	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		s.writer.Add(proj.ID, proj.ID, trace.Transaction{
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

	// Подсказка на «Пороге Apdex» (значение в мс) должна объяснять порог, не индекс
	// 0..1 — страница эндпойнта индекс вообще не показывает.
	if !strings.Contains(string(body), "Порог Apdex T — целевое время ответа") {
		t.Fatalf("GET %s (owner) missing the apdex threshold tooltip: %s", txPath, body)
	}
	if strings.Contains(string(body), "индекс удовлетворённости от 0 до 1") {
		t.Fatalf("GET %s (owner) apdex threshold tooltip still describes the 0..1 index: %s", txPath, body)
	}

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

	resp = getWithCookie(t, s.srv, txPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", txPath, resp.StatusCode)
	}
}

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

	const total = 120
	base := time.Now().UTC().Add(-10 * time.Minute)
	for i := 0; i < total; i++ {
		at := base.Add(time.Duration(i) * time.Millisecond)
		s.writer.Add(proj.ID, proj.ID, trace.Transaction{
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
	rows := strings.Count(string(body), "/performance/")
	if rows > 100 {
		t.Fatalf("rendered %d endpoint rows, want at most 100 (list not truncated)", rows)
	}
	if !strings.Contains(string(body), "Показаны первые") {
		t.Fatalf("missing truncation notice for %d endpoints: %s", total, body)
	}
}

// «Истёкшая» ссылка зависит от h.SpanRetentionDays (настраиваемого TTL spans), а
// не от захардкоженной константы.
func TestWebEndpointDetailSlowestExpiryConfigurable(t *testing.T) {
	s := newPerfStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perfexp-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perfexp-co", "PerfExp Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	proj, err := s.org.CreateProject(context.Background(), o.ID, "perfexp-proj", "PerfExp Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	const transaction = "GET /api/old"
	const trace40 = "perfexp-trace-40d"
	const trace10 = "perfexp-trace-10d"
	now := time.Now().UTC()
	at40 := now.Add(-40 * 24 * time.Hour)
	at10 := now.Add(-10 * 24 * time.Hour)
	s.writer.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID: trace40, SpanID: "perfexp-span-40d", Name: transaction, Op: "http.server",
		Status: "ok", Start: at40, End: at40.Add(50 * time.Millisecond), Environment: "production",
	})
	s.writer.Add(proj.ID, proj.ID, trace.Transaction{
		TraceID: trace10, SpanID: "perfexp-span-10d", Name: transaction, Op: "http.server",
		Status: "ok", Start: at10, End: at10.Add(40 * time.Millisecond), Environment: "production",
	})
	s.flush(t)

	// Дефолтные 24ч (perfDefaultPeriod) не захватили бы эти трейсы.
	start := now.Add(-45 * 24 * time.Hour).Format("2006-01-02T15:04")
	txPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/performance/" +
		url.PathEscape(transaction) + "?period=custom&start=" + start

	link40 := `<a href="/traces/` + trace40
	link10 := `<a href="/traces/` + trace10

	s.h.SpanRetentionDays = 90
	resp := getWithCookie(t, s.srv, txPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (retention=90) status = %d: %s", txPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), link40) {
		t.Fatalf("GET %s (retention=90): трейс 40д должен остаться кликабельным: %s", txPath, body)
	}

	s.h.SpanRetentionDays = 7
	resp = getWithCookie(t, s.srv, txPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (retention=7) status = %d: %s", txPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), link10) {
		t.Fatalf("GET %s (retention=7): трейс 10д должен потерять ссылку: %s", txPath, body)
	}
	if !strings.Contains(string(body), trace10) {
		t.Fatalf("GET %s (retention=7): trace_id 10д должен остаться текстом: %s", txPath, body)
	}

	s.h.SpanRetentionDays = 0
	resp = getWithCookie(t, s.srv, txPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (retention=0) status = %d: %s", txPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), link10) || !strings.Contains(string(body), link40) {
		t.Fatalf("GET %s (retention=0): ни один trace_id не должен терять ссылку: %s", txPath, body)
	}
}

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
