package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebOnboardingFlow — сквозной сценарий задачи 5: регистрация →
// онбординг (организация + проект + ключ) → страница подключения SDK →
// навигация по проектам.
func TestWebOnboardingFlow(t *testing.T) {
	s := newStack(t)

	// Регистрация нового юзера — сразу залогинен.
	regForm := url.Values{
		"email":     {"onboard-user@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}
	resp := postForm(t, s.srv, "/register", regForm, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	cookie := sessionCookie(resp)
	if cookie == nil {
		t.Fatalf("register did not set session cookie")
	}

	// GET /onboarding → 200 + форма (у юзера ещё нет организаций).
	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+"/onboarding", nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get /onboarding: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /onboarding status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<form") {
		t.Fatalf("GET /onboarding body has no <form: %s", body)
	}

	// POST /onboarding с невалидным org slug → 422 с перерисованной формой.
	badForm := url.Values{
		"org_slug":     {"Bad!"},
		"org_name":     {"Bad Org"},
		"project_slug": {"proj"},
		"project_name": {"Proj"},
		"platform":     {"go"},
	}
	resp = postForm(t, s.srv, "/onboarding", badForm, s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /onboarding (bad slug) status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<form") {
		t.Fatalf("POST /onboarding (bad slug) body has no <form: %s", body)
	}

	// POST /onboarding без Origin → 403.
	resp = postForm(t, s.srv, "/onboarding", badForm, "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /onboarding (no origin) status = %d, want 403", resp.StatusCode)
	}

	// POST /onboarding валидный → 303 на /projects/{id}/setup.
	validForm := url.Values{
		"org_slug":     {"acme"},
		"org_name":     {"Acme Inc"},
		"project_slug": {"backend"},
		"project_name": {"Backend"},
		"platform":     {"go"},
	}
	resp = postForm(t, s.srv, "/onboarding", validForm, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /onboarding (valid) status = %d, want 303", resp.StatusCode)
	}
	setupPath := resp.Header.Get("Location")
	if !strings.HasPrefix(setupPath, "/projects/") || !strings.HasSuffix(setupPath, "/setup") {
		t.Fatalf("POST /onboarding (valid) Location = %q, want /projects/{id}/setup", setupPath)
	}
	projectIDStr := strings.TrimSuffix(strings.TrimPrefix(setupPath, "/projects/"), "/setup")
	projectID, err := strconv.ParseInt(projectIDStr, 10, 64)
	if err != nil {
		t.Fatalf("parse project id from %q: %v", setupPath, err)
	}

	// Достаём публичный ключ проекта напрямую из БД для сверки с DSN на
	// странице setup.
	orgSvc := org.NewService(s.pool, 1_000_000)
	keys, err := orgSvc.KeysForProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("keys for project: %v", err)
	}
	if len(keys) != 1 || keys[0].Revoked {
		t.Fatalf("keys for project = %+v, want exactly one live key", keys)
	}
	publicKey := keys[0].PublicKey

	// GET /projects/{id}/setup → 200, содержит DSN с public_key проекта.
	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+setupPath, nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get setup: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", setupPath, resp.StatusCode)
	}
	wantDSN := "://" + publicKey + "@"
	if !strings.Contains(string(body), wantDSN) {
		t.Fatalf("GET %s body missing DSN %q: %s", setupPath, wantDSN, body)
	}

	// GET / теперь → 303 на /projects/{id}/issues (проект уже есть).
	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+"/", nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET / status = %d, want 303", resp.StatusCode)
	}
	wantIssues := "/projects/" + projectIDStr + "/issues"
	if got := resp.Header.Get("Location"); got != wantIssues {
		t.Fatalf("GET / Location = %q, want %q", got, wantIssues)
	}

	// GET /onboarding теперь (у юзера уже есть организация) → 303 на /.
	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+"/onboarding", nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get /onboarding (has org): %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /onboarding (has org) status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Fatalf("GET /onboarding (has org) Location = %q, want /", got)
	}

	// GET /projects → 200, содержит ссылку на созданный проект.
	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+"/projects", nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get /projects: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /projects status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), setupPath) {
		t.Fatalf("GET /projects body missing link %q: %s", setupPath, body)
	}
	if !strings.Contains(string(body), "Backend") {
		t.Fatalf("GET /projects body missing project name: %s", body)
	}
	// Project name now links to the issues list, not straight to setup
	// (fix 2: issues of non-first projects were otherwise unreachable).
	issuesLinkPath := "/projects/" + projectIDStr + "/issues"
	if !strings.Contains(string(body), issuesLinkPath) {
		t.Fatalf("GET /projects body missing link to issues %q: %s", issuesLinkPath, body)
	}
	// Logout must be reachable: the header renders the logged-in user's
	// email and a logout form once userEmail is wired through (fix 1).
	if !strings.Contains(string(body), "onboard-user@example.com") {
		t.Fatalf("GET /projects body missing user email: %s", body)
	}
	if !strings.Contains(string(body), `action="/logout"`) {
		t.Fatalf("GET /projects body missing logout form: %s", body)
	}

	// POST /onboarding с валидным org slug, но невалидным project slug →
	// 422, БЕЗ сиротской организации (баг: раньше CreateOrg успевал
	// закоммититься до провала CreateProject), форма сохраняет org_slug.
	orphanForm := url.Values{
		"org_slug":     {"orphan-check"},
		"org_name":     {"Orphan Check"},
		"project_slug": {"Bad Project!"},
		"project_name": {"Bad Project"},
		"platform":     {"go"},
	}
	resp = postForm(t, s.srv, "/onboarding", orphanForm, s.srv.URL, cookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST /onboarding (bad project slug) status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(string(body), `value="orphan-check"`) {
		t.Fatalf("POST /onboarding (bad project slug) body does not preserve org_slug: %s", body)
	}
	var orphanCount int
	if err := s.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM organizations WHERE slug = $1", "orphan-check").Scan(&orphanCount); err != nil {
		t.Fatalf("count orphan orgs: %v", err)
	}
	if orphanCount != 0 {
		t.Fatalf("orphan org left behind: count = %d, want 0", orphanCount)
	}

	// POST /onboarding с непроверенной платформой → 303, платформа в БД
	// нормализуется на "other".
	hax0rForm := url.Values{
		"org_slug":     {"hax0r-org"},
		"org_name":     {"Hax0r Org"},
		"project_slug": {"hax0r-proj"},
		"project_name": {"Hax0r Proj"},
		"platform":     {"hax0r"},
	}
	resp = postForm(t, s.srv, "/onboarding", hax0rForm, s.srv.URL, cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /onboarding (hax0r platform) status = %d, want 303", resp.StatusCode)
	}
	var gotPlatform string
	if err := s.pool.QueryRow(context.Background(),
		"SELECT platform FROM projects WHERE slug = $1", "hax0r-proj").Scan(&gotPlatform); err != nil {
		t.Fatalf("query platform: %v", err)
	}
	if gotPlatform != "other" {
		t.Fatalf("platform in DB = %q, want %q", gotPlatform, "other")
	}

	// GET /projects/{id}/setup от юзера без доступа к проекту → 404.
	otherForm := url.Values{
		"email":     {"other-user@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}
	resp = postForm(t, s.srv, "/register", otherForm, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	otherCookie := sessionCookie(resp)
	if otherCookie == nil {
		t.Fatalf("second register did not set session cookie")
	}

	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+setupPath, nil)
	req.AddCookie(otherCookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get setup as other user: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (other user) status = %d, want 404", setupPath, resp.StatusCode)
	}
}
