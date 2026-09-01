package web_test

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestOrgProjectsListsOnlyThatOrg — /orgs/{id}/projects показывает проекты
// только этой организации: тенант-изоляция закрывается тестом, а не только
// комментарием (ProjectsForUserInOrg уже проверена отдельно, internal/org).
func TestOrgProjectsListsOnlyThatOrg(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "orgproj@example.com")
	pa := createProject(t, s, uid, "org-a", "proj-a")
	pb := createProject(t, s, uid, "org-b", "proj-b")

	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(pa.OrgID, 10)+"/projects", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), pb.Name) {
		t.Errorf("страница организации A показывает проект %q организации B", pb.Name)
	}
}

// TestOrgProjectsForeignOrgIs404 — чужая организация отдаёт 404, а не пустой
// список: адрес не должен подтверждать её существование.
func TestOrgProjectsForeignOrgIs404(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "member@example.com")
	_ = createProject(t, s, uid, "own-org", "own-proj")
	// организация, в которой пользователь не состоит
	other, _ := registerAndLogin(t, s, "stranger@example.com")
	foreign := createProject(t, s, other, "foreign-org", "foreign-proj")

	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(foreign.OrgID, 10)+"/projects", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: адрес не должен подтверждать существование чужой организации", resp.StatusCode)
	}
}

// TestProjectsRedirectNoOrgGoesToOnboarding — GET /projects у юзера без
// единой организации (раньше рендерил пустой плоский список, редиректить
// теперь некуда) уводит на /onboarding — тот же тупик, что и у index() для
// свежезарегистрированного юзера без проектов.
func TestProjectsRedirectNoOrgGoesToOnboarding(t *testing.T) {
	s := newIssuesStack(t)
	_, cookie := registerAndLogin(t, s, "noorg-redirect@example.com")

	resp := getWithCookie(t, s.srv, "/projects", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/onboarding" {
		t.Fatalf("Location = %q, want /onboarding", got)
	}
}

// TestProjectsRedirectFallsBackToFirstOrgByName — без запомненного проекта
// GET /projects ведёт на первую организацию по порядку OrgsOf (по имени).
func TestProjectsRedirectFallsBackToFirstOrgByName(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "fallback-redirect@example.com")
	pa := createProject(t, s, uid, "aaa-fb-org", "aaa-fb-proj")
	_ = createProject(t, s, uid, "zzz-fb-org", "zzz-fb-proj")

	resp := getWithCookie(t, s.srv, "/projects", cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	want := "/orgs/" + strconv.FormatInt(pa.OrgID, 10) + "/projects"
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (первая организация по имени)", got, want)
	}
}

// TestProjectsRedirectRemembersCookieOrgOverFirst — с несколькими
// организациями запомненный проект (cookie "proj", см. projcookie.go)
// перевешивает алфавитный порядок OrgsOf: GET /projects ведёт на
// организацию запомненного проекта, а не на первую по имени.
func TestProjectsRedirectRemembersCookieOrgOverFirst(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "remember-redirect@example.com")
	_ = createProject(t, s, uid, "aaa-rem-org", "aaa-rem-proj") // первая по имени
	pb := createProject(t, s, uid, "bbb-rem-org", "bbb-rem-proj")

	// Заходим в проект B — withShell запоминает его в cookie "proj"
	// (см. shell.go, projcookie.go).
	visit := getWithCookie(t, s.srv, "/projects/"+strconv.FormatInt(pb.ID, 10)+"/issues", cookie)
	io.Copy(io.Discard, visit.Body)
	visit.Body.Close()
	if visit.StatusCode != http.StatusOK {
		t.Fatalf("visit issues status = %d, want 200", visit.StatusCode)
	}
	var projCookie *http.Cookie
	for _, c := range visit.Cookies() {
		if c.Name == "proj" {
			projCookie = c
		}
	}
	if projCookie == nil {
		t.Fatalf("посещение issues проекта B не выставило cookie proj")
	}

	req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/projects", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(cookie)
	req.AddCookie(projCookie)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	want := "/orgs/" + strconv.FormatInt(pb.OrgID, 10) + "/projects"
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (организация запомненного проекта B, не первая по имени A)", got, want)
	}
}
