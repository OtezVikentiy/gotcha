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

	"github.com/jackc/pgx/v5/pgxpool"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
	"gitflic.ru/otezvikentiy/gotcha/internal/testenv"
	"gitflic.ru/otezvikentiy/gotcha/internal/trace"
	"gitflic.ru/otezvikentiy/gotcha/internal/web"
)

// perfIssuesStack — стенд страниц perf-проблем: только PG (страницы читают
// trace.IssueService, CH здесь не нужен — Events не используется).
type perfIssuesStack struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
	org  *org.Service
	auth *auth.Service
	perf *trace.IssueService
}

func newPerfIssuesStack(t *testing.T) *perfIssuesStack {
	t.Helper()
	pool := testenv.MigratedPG(t)

	authSvc := auth.NewService(pool)
	orgSvc := org.NewService(pool, 1_000_000)
	perfSvc := trace.NewIssueService(pool)

	mux := http.NewServeMux()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	h := web.New(authSvc, orgSvc, nil, nil, srv.URL)
	h.PerfIssues = perfSvc
	h.Register(mux)

	return &perfIssuesStack{pool: pool, srv: srv, org: orgSvc, auth: authSvc, perf: perfSvc}
}

// insertPerfIssue кладёт строку perf_issues напрямую (детерминированно, с точно
// заданным evidence). Возвращает id новой проблемы.
func (s *perfIssuesStack) insertPerfIssue(t *testing.T, projectID, count int64, kind, fingerprint, title, culprit, status, sampleTrace, evidence string) int64 {
	t.Helper()
	var id int64
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO perf_issues (project_id, fingerprint, kind, title, culprit, status, count, sample_trace_id, evidence)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb) RETURNING id`,
		projectID, fingerprint, kind, title, culprit, status, count, sampleTrace, evidence).Scan(&id)
	if err != nil {
		t.Fatalf("insert perf issue %q: %v", title, err)
	}
	return id
}

func (s *perfIssuesStack) statusOf(t *testing.T, id int64) string {
	t.Helper()
	var status string
	if err := s.pool.QueryRow(context.Background(),
		"SELECT status FROM perf_issues WHERE id = $1", id).Scan(&status); err != nil {
		t.Fatalf("read status of %d: %v", id, err)
	}
	return status
}

func TestWebPerfIssuesList(t *testing.T) {
	s := newPerfIssuesStack(t)

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perf-list-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perf-list-co", "Perf List Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(context.Background(), o.ID, "perf-list-proj", "Perf List Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	nPlusEv := `{"count":8,"total_us":24000,"parent_op":"http.server","span_ids":["a","b"]}`
	s.insertPerfIssue(t, project.ID, 8, trace.KindNPlusOne, "fp-nplus",
		"N+1 запросов: SELECT * FROM users", "GET /orders", "unresolved", "trace-abc", nPlusEv)
	s.insertPerfIssue(t, project.ID, 3, trace.KindSlowDBQuery, "fp-slow",
		"Медленный запрос: SELECT big", "GET /report", "resolved", "", `{"count":3,"max_us":700000}`)

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/perf-issues"

	// Дефолт (unresolved) показывает только unresolved N+1, kind человекочитаемо,
	// count и статус.
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", listPath, resp.StatusCode, body)
	}
	bs := string(body)
	if !strings.Contains(bs, "N+1 запросов: SELECT * FROM users") {
		t.Fatalf("list missing N+1 title: %s", bs)
	}
	if !strings.Contains(bs, "N+1 queries") {
		t.Fatalf("list missing human-readable kind: %s", bs)
	}
	if !strings.Contains(bs, "GET /orders") {
		t.Fatalf("list missing culprit: %s", bs)
	}
	if strings.Contains(bs, "Медленный запрос: SELECT big") {
		t.Fatalf("default (unresolved) filter leaked resolved issue: %s", bs)
	}

	// ?status=resolved показывает только resolved slow-query.
	resp = getWithCookie(t, s.srv, listPath+"?status=resolved", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s?status=resolved status = %d, want 200", listPath, resp.StatusCode)
	}
	bs = string(body)
	if !strings.Contains(bs, "Медленный запрос: SELECT big") {
		t.Fatalf("?status=resolved missing resolved issue: %s", bs)
	}
	if strings.Contains(bs, "N+1 запросов: SELECT * FROM users") {
		t.Fatalf("?status=resolved leaked unresolved issue: %s", bs)
	}

	// ?status=all показывает обе.
	resp = getWithCookie(t, s.srv, listPath+"?status=all", ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	bs = string(body)
	if !strings.Contains(bs, "N+1 запросов: SELECT * FROM users") || !strings.Contains(bs, "Медленный запрос: SELECT big") {
		t.Fatalf("?status=all missing one of the issues: %s", bs)
	}

	// Чужой юзер (не участник) → 404.
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "perf-list-outsider@example.com")
	resp = getWithCookie(t, s.srv, listPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (outsider) status = %d, want 404", listPath, resp.StatusCode)
	}
}

func TestWebPerfIssuesListEmpty(t *testing.T) {
	s := newPerfIssuesStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perf-empty-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perf-empty-co", "Perf Empty Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(context.Background(), o.ID, "perf-empty-proj", "Perf Empty Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	listPath := "/projects/" + strconv.FormatInt(project.ID, 10) + "/perf-issues"
	resp := getWithCookie(t, s.srv, listPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", listPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "no performance issues detected") {
		t.Fatalf("empty list missing placeholder: %s", body)
	}
}

func TestWebPerfIssueDetailAndEvidence(t *testing.T) {
	s := newPerfIssuesStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perf-detail-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perf-detail-co", "Perf Detail Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(context.Background(), o.ID, "perf-detail-proj", "Perf Detail Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	// total_us=24000 → 24.0ms, count=8.
	ev := `{"count":8,"total_us":24000,"parent_op":"http.server","span_ids":["a"]}`
	id := s.insertPerfIssue(t, project.ID, 8, trace.KindNPlusOne, "fp-detail",
		"N+1 запросов: SELECT * FROM users WHERE id = ?", "GET /orders", "unresolved", "trace-xyz", ev)

	detailPath := "/perf-issues/" + strconv.FormatInt(id, 10)
	resp := getWithCookie(t, s.srv, detailPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", detailPath, resp.StatusCode, body)
	}
	bs := string(body)
	if !strings.Contains(bs, "N+1 запросов: SELECT * FROM users WHERE id = ?") {
		t.Fatalf("detail missing title: %s", bs)
	}
	if !strings.Contains(bs, "N+1 queries") {
		t.Fatalf("detail missing human-readable kind: %s", bs)
	}
	// Evidence: repeat count 8 и total 24.0ms.
	if !strings.Contains(bs, "24.0ms") {
		t.Fatalf("detail evidence missing total time in ms: %s", bs)
	}
	// Ссылка на пример трейса.
	if !strings.Contains(bs, "/traces/trace-xyz") {
		t.Fatalf("detail missing sample trace link: %s", bs)
	}
	// Owner видит кнопки статуса.
	if !strings.Contains(bs, `value="resolved"`) {
		t.Fatalf("owner detail missing resolve button: %s", bs)
	}

	// Несуществующий id → 404.
	resp = getWithCookie(t, s.srv, "/perf-issues/999999", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET nonexistent detail status = %d, want 404", resp.StatusCode)
	}
}

func TestWebPerfIssueSetStatus(t *testing.T) {
	s := newPerfIssuesStack(t)
	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "perf-status-owner@example.com")
	o, err := s.org.CreateOrg(context.Background(), "perf-status-co", "Perf Status Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(context.Background(), o.ID, "perf-status-proj", "Perf Status Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	id := s.insertPerfIssue(t, project.ID, 5, trace.KindSlowDBQuery, "fp-status",
		"Медленный запрос", "GET /slow", "unresolved", "", `{"count":5,"max_us":600000}`)
	statusPath := "/perf-issues/" + strconv.FormatInt(id, 10) + "/status"

	// Resolve → 303, статус в БД resolved.
	resp := postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST resolve status = %d, want 303", resp.StatusCode)
	}
	if got := s.statusOf(t, id); got != "resolved" {
		t.Fatalf("after resolve status = %q, want resolved", got)
	}

	// Ignore → 303, статус ignored.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"ignored"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST ignore status = %d, want 303", resp.StatusCode)
	}
	if got := s.statusOf(t, id); got != "ignored" {
		t.Fatalf("after ignore status = %q, want ignored", got)
	}

	// Неизвестный статус → 422.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"bogus"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST bogus status = %d, want 422", resp.StatusCode)
	}

	// Без same-origin → 403.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST no-origin status = %d, want 403", resp.StatusCode)
	}
}

func TestWebPerfIssueMemberCannotManage(t *testing.T) {
	s := newPerfIssuesStack(t)
	ownerID, _ := orgSettingsRegister(t, s.auth, "perf-member-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "perf-member-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), "perf-member-co", "Perf Member Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	project, err := s.org.CreateProject(context.Background(), o.ID, "perf-member-proj", "Perf Member Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Доступ к проекту у члена — через команду (member видит только проекты команд).
	addTeamAccess(t, s.org, o.ID, project.ID, memberID, "perf-member-team")

	id := s.insertPerfIssue(t, project.ID, 5, trace.KindSlowDBQuery, "fp-member",
		"Медленный запрос", "GET /slow", "unresolved", "", `{"count":5,"max_us":600000}`)
	detailPath := "/perf-issues/" + strconv.FormatInt(id, 10)
	statusPath := detailPath + "/status"

	// Member видит страницу, но НЕ видит кнопок статуса.
	resp := getWithCookie(t, s.srv, detailPath, memberCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET %s status = %d, want 200: %s", detailPath, resp.StatusCode, body)
	}
	if strings.Contains(string(body), `name="status"`) {
		t.Fatalf("member must not see status buttons: %s", body)
	}

	// Member POST статус → 404 (requireProjectRole).
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("member POST status = %d, want 404", resp.StatusCode)
	}
	if got := s.statusOf(t, id); got != "unresolved" {
		t.Fatalf("member POST changed status to %q, want unresolved", got)
	}
}

func TestWebPerfIssueForeignProject(t *testing.T) {
	s := newPerfIssuesStack(t)
	ownerID, _ := orgSettingsRegister(t, s.auth, "perf-foreign-owner@example.com")
	_, outsiderCookie := orgSettingsRegister(t, s.auth, "perf-foreign-outsider@example.com")

	o, err := s.org.CreateOrg(context.Background(), "perf-foreign-co", "Perf Foreign Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.org.CreateProject(context.Background(), o.ID, "perf-foreign-proj", "Perf Foreign Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	id := s.insertPerfIssue(t, project.ID, 5, trace.KindSlowDBQuery, "fp-foreign",
		"Медленный запрос", "GET /slow", "unresolved", "", `{"count":5,"max_us":600000}`)
	detailPath := "/perf-issues/" + strconv.FormatInt(id, 10)
	statusPath := detailPath + "/status"

	// Чужой юзер: GET страницы проблемы → 404 (не палим существование id).
	resp := getWithCookie(t, s.srv, detailPath, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider GET detail status = %d, want 404", resp.StatusCode)
	}

	// Чужой юзер: POST статус → 404.
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, outsiderCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider POST status = %d, want 404", resp.StatusCode)
	}
	if got := s.statusOf(t, id); got != "unresolved" {
		t.Fatalf("outsider POST changed status to %q", got)
	}
}
