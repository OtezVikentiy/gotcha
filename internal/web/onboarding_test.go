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

func TestWebOnboardingFlow(t *testing.T) {
	s := newStack(t)

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

	resp = postForm(t, s.srv, "/onboarding", badForm, "", cookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST /onboarding (no origin) status = %d, want 403", resp.StatusCode)
	}

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

	orgSvc := org.NewService(s.pool, 1_000_000)
	keys, err := orgSvc.KeysForProject(context.Background(), projectID)
	if err != nil {
		t.Fatalf("keys for project: %v", err)
	}
	// Все языковые сниппеты рендерятся на странице, включая JS с browser-DSN,
	// хотя платформа проекта — go; его и сверяем ниже.
	if len(keys) != 3 {
		t.Fatalf("keys for project = %+v, want exactly three keys", keys)
	}
	wantKinds := []org.KeyKind{org.KindBrowser, org.KindServer, org.KindAgent}
	for i, k := range keys {
		if k.Revoked {
			t.Fatalf("keys for project = %+v, want no revoked keys", keys)
		}
		if k.Kind != wantKinds[i] {
			t.Fatalf("keys for project = %+v, want kinds %v", keys, wantKinds)
		}
	}
	publicKey := keys[0].PublicKey

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

	orgID, err := orgSvc.ProjectOrg(context.Background(), projectID)
	if err != nil {
		t.Fatalf("project org: %v", err)
	}
	orgProjectsPath := "/orgs/" + strconv.FormatInt(orgID, 10) + "/projects"

	// Запрос несёт только сессионную cookie — тест не гоняет cookie jar,
	// поэтому cookie "proj" от /setup сюда не долетает.
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
	if got := resp.Header.Get("Location"); got != orgProjectsPath {
		t.Fatalf("GET / Location = %q, want %q", got, orgProjectsPath)
	}
	// Редирект обязан резолвиться: сверяем реальным GET, а не только адресом в Location.
	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+orgProjectsPath, nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", orgProjectsPath, err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (target of GET / redirect) status = %d, want 200: %s", orgProjectsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Backend") {
		t.Fatalf("GET %s body missing the onboarded project: %s", orgProjectsPath, body)
	}

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

	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+"/projects", nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get /projects: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /projects status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != orgProjectsPath {
		t.Fatalf("GET /projects Location = %q, want %q", got, orgProjectsPath)
	}

	req, _ = http.NewRequest(http.MethodGet, s.srv.URL+orgProjectsPath, nil)
	req.AddCookie(cookie)
	resp, err = noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", orgProjectsPath, err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", orgProjectsPath, resp.StatusCode)
	}
	if !strings.Contains(string(body), "Backend") {
		t.Fatalf("GET %s body missing project name: %s", orgProjectsPath, body)
	}
	overviewLinkPath := "/projects/" + projectIDStr + "/overview"
	if !strings.Contains(string(body), overviewLinkPath) {
		t.Fatalf("GET %s body missing link to overview %q: %s", orgProjectsPath, overviewLinkPath, body)
	}
	if !strings.Contains(string(body), setupPath) {
		t.Fatalf("GET %s body missing link %q: %s", orgProjectsPath, setupPath, body)
	}
	if !strings.Contains(string(body), "onboard-user@example.com") {
		t.Fatalf("GET /projects body missing user email: %s", body)
	}
	if !strings.Contains(string(body), `action="/logout"`) {
		t.Fatalf("GET /projects body missing logout form: %s", body)
	}

	// От ВТОРОГО пользователя: у первого уже есть проект, а онбординг доступен
	// только тому, у кого нет ни одного.
	secondReg := url.Values{
		"email":     {"onboard-second@example.com"},
		"password":  {"correct-horse-battery"},
		"password2": {"correct-horse-battery"},
	}
	resp = postForm(t, s.srv, "/register", secondReg, s.srv.URL, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	secondCookie := sessionCookie(resp)
	if secondCookie == nil {
		t.Fatalf("register (second user) did not set session cookie")
	}

	orphanForm := url.Values{
		"org_slug":     {"orphan-check"},
		"org_name":     {"Orphan Check"},
		"project_slug": {"Bad Project!"},
		"project_name": {"Bad Project"},
		"platform":     {"go"},
	}
	resp = postForm(t, s.srv, "/onboarding", orphanForm, s.srv.URL, secondCookie)
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

	hax0rForm := url.Values{
		"org_slug":     {"hax0r-org"},
		"org_name":     {"Hax0r Org"},
		"project_slug": {"hax0r-proj"},
		"project_name": {"Hax0r Proj"},
		"platform":     {"hax0r"},
	}
	resp = postForm(t, s.srv, "/onboarding", hax0rForm, s.srv.URL, secondCookie)
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

func TestProjectSetupShowsSnippetsWithoutPlatformDSN(t *testing.T) {
	s := newStack(t)
	ctx := context.Background()

	uid, cookie := orgSettingsRegister(t, s.h.Auth, "setup-fallback@example.com")
	o, err := s.h.Org.CreateOrg(ctx, "setup-fb", "Setup FB", uid)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	project, err := s.h.Org.CreateProject(ctx, o.ID, "js-proj", "JS Proj", "javascript")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	keys, err := s.h.Org.CreateKeys(ctx, project.ID, org.KindBrowser, org.KindServer)
	if err != nil {
		t.Fatalf("create keys: %v", err)
	}
	var browserKeyID int64
	var serverKey string
	for _, k := range keys {
		switch k.Kind {
		case org.KindBrowser:
			browserKeyID = k.ID
		case org.KindServer:
			serverKey = k.PublicKey
		}
	}
	if browserKeyID == 0 || serverKey == "" {
		t.Fatalf("keys = %+v, want один browser и один server", keys)
	}
	if err := s.h.Org.RevokeKey(ctx, browserKeyID); err != nil {
		t.Fatalf("revoke browser key: %v", err)
	}

	setupPath := projectSetupPathForTest(project.ID)
	req, _ := http.NewRequest(http.MethodGet, s.srv.URL+setupPath, nil)
	req.AddCookie(cookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("get setup: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", setupPath, resp.StatusCode)
	}
	if strings.Contains(string(body), "нет активного ключа") {
		t.Errorf("страница показывает пустое состояние, хотя server-ключ жив: %s", body)
	}
	wantServerDSN := "://" + serverKey + "@"
	if !strings.Contains(string(body), wantServerDSN) {
		t.Fatalf("GET %s body missing server DSN %q (Go/PHP/Python сниппеты должны остаться): %s", setupPath, wantServerDSN, body)
	}
	// Карточка DSN, подсказки и панели сниппетов — в обёртке вертикального
	// ритма .card-stack: без неё section.card/.panel идут впритык.
	if !strings.Contains(string(body), `<div class="card-stack">`) {
		t.Errorf("GET %s: нет обёртки card-stack вокруг блоков setup", setupPath)
	}
	// Ищем по отсутствию команды установки, а не по кавычкам dsn: "" — шаблон их HTML-экранирует.
	if strings.Contains(string(body), "npm install @sentry/browser") {
		t.Errorf("страница показывает JS-сниппет с пустым DSN (ловушка копирования): %s", body)
	}
	if !strings.Contains(string(body), "Для JavaScript нужен ключ типа «Браузер»") {
		t.Errorf("GET %s не объясняет пропажу JS-сниппета: %s", setupPath, body)
	}
	if !strings.Contains(string(body), "Настройки проекта") {
		t.Errorf("GET %s: подсказка без ссылки «Настройки проекта»: %s", setupPath, body)
	}

	project2, err := s.h.Org.CreateProject(ctx, o.ID, "js-proj-empty", "JS Proj Empty", "javascript")
	if err != nil {
		t.Fatalf("create project 2: %v", err)
	}
	setupPath2 := projectSetupPathForTest(project2.ID)
	req2, _ := http.NewRequest(http.MethodGet, s.srv.URL+setupPath2, nil)
	req2.AddCookie(cookie)
	resp2, err := noRedirectClient().Do(req2)
	if err != nil {
		t.Fatalf("get setup 2: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", setupPath2, resp2.StatusCode)
	}
	if !strings.Contains(string(body2), "нет активного ключа") {
		t.Errorf("GET %s без ключей должен показывать пустое состояние: %s", setupPath2, body2)
	}

	// Шапочный DSN пуст (для go он берётся из server), но JS-сниппет с browser-DSN должен остаться.
	project3, err := s.h.Org.CreateProject(ctx, o.ID, "go-proj", "Go Proj", "go")
	if err != nil {
		t.Fatalf("create project 3: %v", err)
	}
	keys3, err := s.h.Org.CreateKeys(ctx, project3.ID, org.KindBrowser, org.KindServer)
	if err != nil {
		t.Fatalf("create keys 3: %v", err)
	}
	var serverKeyID3 int64
	var browserKey3 string
	for _, k := range keys3 {
		switch k.Kind {
		case org.KindServer:
			serverKeyID3 = k.ID
		case org.KindBrowser:
			browserKey3 = k.PublicKey
		}
	}
	if serverKeyID3 == 0 || browserKey3 == "" {
		t.Fatalf("keys3 = %+v, want один browser и один server", keys3)
	}
	if err := s.h.Org.RevokeKey(ctx, serverKeyID3); err != nil {
		t.Fatalf("revoke server key: %v", err)
	}

	setupPath3 := projectSetupPathForTest(project3.ID)
	req3, _ := http.NewRequest(http.MethodGet, s.srv.URL+setupPath3, nil)
	req3.AddCookie(cookie)
	resp3, err := noRedirectClient().Do(req3)
	if err != nil {
		t.Fatalf("get setup 3: %v", err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", setupPath3, resp3.StatusCode)
	}
	if strings.Contains(string(body3), "нет активного ключа") {
		t.Errorf("страница показывает пустое состояние, хотя browser-ключ жив: %s", body3)
	}
	wantBrowserDSN := "://" + browserKey3 + "@"
	if !strings.Contains(string(body3), wantBrowserDSN) {
		t.Fatalf("GET %s body missing browser DSN %q (JS-сниппет должен остаться): %s", setupPath3, wantBrowserDSN, body3)
	}
	if strings.Contains(string(body3), "go get github.com/getsentry/sentry-go") {
		t.Errorf("страница показывает Go-сниппет с пустым DSN (ловушка копирования): %s", body3)
	}
	if !strings.Contains(string(body3), "Для Go нужен ключ типа «Сервер»") {
		t.Errorf("GET %s не объясняет пропажу Go-сниппета: %s", setupPath3, body3)
	}
	if !strings.Contains(string(body3), "Настройки проекта") {
		t.Errorf("GET %s: подсказка без ссылки «Настройки проекта»: %s", setupPath3, body3)
	}
}

func projectSetupPathForTest(projectID int64) string {
	return "/projects/" + strconv.FormatInt(projectID, 10) + "/setup"
}
