package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/event"
	"gitflic.ru/otezvikentiy/gotcha/internal/issue"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// issuesStack — в отличие от newStack (auth_test.go), поднимает и PG, и CH:
// страницы issues читают event.Query.Sparklines, поэтому Events == nil здесь
// недопустим (в задаче 4 CH вообще не трогался).
type issuesStack struct {
	pool    *pgxpool.Pool
	srv     *httptest.Server
	org     *org.Service
	auth    *auth.Service
	issues  *issue.Service
	batcher *event.Batcher
}

func newIssuesStack(t *testing.T) *issuesStack {
	t.Helper()
	pool := testenv.MigratedPG(t)
	ch := testenv.MigratedCH(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	issueSvc := issue.NewService(pool)
	eventsQuery := event.NewQuery(ch)
	batcher := event.NewBatcher(ch)
	go batcher.Run()

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = batcher.Close(ctx)
	})

	h := web.New(authSvc, orgSvc, issueSvc, eventsQuery, srv.URL)
	h.Register(mux)

	return &issuesStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, issues: issueSvc, batcher: batcher}
}

// addEvent кладёт событие в батчер; для попадания в спарклайн теста нужен
// отдельный flushEvents, чтобы вставка в CH гарантированно завершилась до
// последующего GET.
func (s *issuesStack) addEvent(projectID, issueID int64, at time.Time) {
	s.batcher.Add(event.Event{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		IssueID:   issueID,
		Timestamp: at,
		Level:     "error",
		Message:   "boom",
		Tags:      map[string]string{},
	})
}

// flushEvents синхронно доливает буфер батчера в CH (аналогично
// TestBatcherInsertsIntoClickHouse), не дожидаясь тикера. Close идемпотентен,
// поэтому повторный вызов в t.Cleanup после этого безопасен.
func (s *issuesStack) flushEvents(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.batcher.Close(ctx); err != nil {
		t.Fatalf("flush events: %v", err)
	}
}

// registerAndLogin — регистрирует нового юзера через auth.Service напрямую
// (без HTTP) и возвращает его id и cookie сессии для последующих запросов.
func registerAndLogin(t *testing.T, s *issuesStack, email string) (int64, *http.Cookie) {
	t.Helper()
	uid, err := s.auth.Register(context.Background(), email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	token, err := s.auth.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("create session for %s: %v", email, err)
	}
	return uid, &http.Cookie{Name: auth.CookieName, Value: token}
}

// createProject — организация + проект, владелец uid.
func createProject(t *testing.T, s *issuesStack, uid int64, orgSlug, projectSlug string) org.Project {
	t.Helper()
	o, err := s.org.CreateOrg(context.Background(), orgSlug, orgSlug, uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	p, err := s.org.CreateProject(context.Background(), o.ID, projectSlug, projectSlug, "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return p
}

func getWithCookie(t *testing.T, srv *httptest.Server, path string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestWebIssuesList(t *testing.T) {
	s := newIssuesStack(t)

	ownerID, ownerCookie := registerAndLogin(t, s, "issues-owner@example.com")
	project := createProject(t, s, ownerID, "issues-org", "issues-proj")

	now := time.Now().UTC()

	// Issue 1: error, times_seen=1.
	r1, err := s.issues.Upsert(context.Background(), project.ID, "fp-error", "NullPointerException", "pkg/a.go:10", "error", now)
	if err != nil {
		t.Fatalf("upsert issue1: %v", err)
	}

	// Issue 2: warning, times_seen=3 (три Upsert увеличивают счётчик).
	var r2 issue.UpsertResult
	for i := 0; i < 3; i++ {
		r2, err = s.issues.Upsert(context.Background(), project.ID, "fp-warning", "Slow query detected", "pkg/b.go:20", "warning", now)
		if err != nil {
			t.Fatalf("upsert issue2: %v", err)
		}
	}

	// Issue 3: info, times_seen=1.
	r3, err := s.issues.Upsert(context.Background(), project.ID, "fp-info", "Deprecated API used", "pkg/c.go:30", "info", now)
	if err != nil {
		t.Fatalf("upsert issue3: %v", err)
	}

	// 2 события в CH для issue1 — должны попасть в спарклайн.
	s.addEvent(project.ID, r1.IssueID, now.Add(-2*time.Hour))
	s.addEvent(project.ID, r1.IssueID, now.Add(-1*time.Hour))
	s.flushEvents(t)

	issuesPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/issues"

	// GET списка → 200, содержит все 3 title и как минимум один <svg (спарклайн).
	resp := getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", issuesPath, resp.StatusCode, body)
	}
	for _, title := range []string{"NullPointerException", "Slow query detected", "Deprecated API used"} {
		if !strings.Contains(string(body), title) {
			t.Fatalf("GET %s body missing title %q: %s", issuesPath, title, body)
		}
	}
	if !strings.Contains(string(body), "<svg") {
		t.Fatalf("GET %s body missing <svg sparkline: %s", issuesPath, body)
	}

	// Resolve issue1, затем ?status=resolved → только он.
	if _, err := s.issues.SetStatusBulk(context.Background(), project.ID, []int64{r1.IssueID}, "resolved"); err != nil {
		t.Fatalf("set status bulk: %v", err)
	}
	resp = getWithCookie(t, s.srv, issuesPath+"?status=resolved", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?status=resolved status = %d, want 200", issuesPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "NullPointerException") {
		t.Fatalf("GET %s?status=resolved missing resolved issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Slow query detected") || strings.Contains(string(body), "Deprecated API used") {
		t.Fatalf("GET %s?status=resolved leaked non-resolved issues: %s", issuesPath, body)
	}

	// ?q= фильтрует по подстроке title/culprit.
	resp = getWithCookie(t, s.srv, issuesPath+"?q=Slow", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?q=Slow status = %d, want 200", issuesPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Slow query detected") {
		t.Fatalf("GET %s?q=Slow missing matching issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Deprecated API used") {
		t.Fatalf("GET %s?q=Slow leaked non-matching issue: %s", issuesPath, body)
	}

	// ?level=warning фильтрует по уровню.
	resp = getWithCookie(t, s.srv, issuesPath+"?level=warning", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?level=warning status = %d, want 200", issuesPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Slow query detected") {
		t.Fatalf("GET %s?level=warning missing matching issue: %s", issuesPath, body)
	}
	if strings.Contains(string(body), "Deprecated API used") || strings.Contains(string(body), "NullPointerException") {
		t.Fatalf("GET %s?level=warning leaked non-matching issue: %s", issuesPath, body)
	}

	// Bulk resolve двух issues → 303, статусы поменялись.
	bulkPath := issuesPath + "/bulk"
	form := url.Values{
		"action": {"resolve"},
		"ids":    {strconv.FormatInt(r2.IssueID, 10), strconv.FormatInt(r3.IssueID, 10)},
	}
	resp = postForm(t, s.srv, bulkPath, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", bulkPath, resp.StatusCode)
	}

	got2, err := s.issues.Get(context.Background(), r2.IssueID)
	if err != nil {
		t.Fatalf("get issue2: %v", err)
	}
	if got2.Status != "resolved" {
		t.Fatalf("issue2 status = %q, want resolved", got2.Status)
	}
	got3, err := s.issues.Get(context.Background(), r3.IssueID)
	if err != nil {
		t.Fatalf("get issue3: %v", err)
	}
	if got3.Status != "resolved" {
		t.Fatalf("issue3 status = %q, want resolved", got3.Status)
	}

	// POST bulk без same-origin Origin/Referer → 403.
	resp = postForm(t, s.srv, bulkPath, form, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", bulkPath, resp.StatusCode)
	}

	// Доступ чужим юзером (не участник организации) → 404.
	_, otherCookie := registerAndLogin(t, s, "issues-outsider@example.com")
	resp = getWithCookie(t, s.srv, issuesPath, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", issuesPath, resp.StatusCode)
	}

	// POST bulk чужим юзером → тоже 404 (не должен видеть/трогать issues проекта).
	resp = postForm(t, s.srv, bulkPath, form, s.srv.URL, otherCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (outsider) status = %d, want 404", bulkPath, resp.StatusCode)
	}
}

func TestBulkRedirectTargetRejectsProtocolRelativePaths(t *testing.T) {
	baseURL := "http://example.com"
	projectID := int64(42)
	expectedFallback := "/projects/42/issues"

	// Test case 1: Protocol-relative path (same host as BaseURL) should be rejected
	req := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com//evil.com/x"},
		},
	}
	got := web.BulkRedirectTarget(req, baseURL, projectID)
	if got != expectedFallback {
		t.Errorf("protocol-relative referer: got %q, want %q", got, expectedFallback)
	}

	// Test case 1b: Backslash-prefixed path (browsers normalize "\" to "/",
	// turning "/\evil.com" into the same protocol-relative "//evil.com" as
	// test case 1) should also be rejected.
	reqBackslash := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com/\\evil.com"},
		},
	}
	gotBackslash := web.BulkRedirectTarget(reqBackslash, baseURL, projectID)
	if gotBackslash != expectedFallback {
		t.Errorf("backslash referer: got %q, want %q", gotBackslash, expectedFallback)
	}

	// Test case 2: Normal referer with path and query should be preserved
	req2 := &http.Request{
		Header: http.Header{
			"Referer": []string{"http://example.com/projects/42/issues?status=resolved&page=2"},
		},
	}
	got2 := web.BulkRedirectTarget(req2, baseURL, projectID)
	expected2 := "/projects/42/issues?status=resolved&page=2"
	if got2 != expected2 {
		t.Errorf("valid referer: got %q, want %q", got2, expected2)
	}
}
