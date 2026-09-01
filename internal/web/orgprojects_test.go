package web_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
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

// TestOrgProjectsScopedToMemberTeams — фикс-раунд 1, п.1: внутри ОДНОЙ
// организации member видит только проекты своих команд, а не все проекты
// организации (ProjectsForUserInOrg сохраняет тот же командный скоуп, что и
// ProjectsForUser — accessCondition). Мутация «подменить
// ProjectsForUserInOrg на ProjectsOf» ни один из прежних фикстур не ловила:
// везде вызывал либо owner (обходит скоуп), либо у организации был ровно
// один проект.
func TestOrgProjectsScopedToMemberTeams(t *testing.T) {
	s := newIssuesStack(t)
	ownerID, _ := registerAndLogin(t, s, "teamscope-owner@example.com")
	memberID, memberCookie := registerAndLogin(t, s, "teamscope-member@example.com")

	o, err := s.org.CreateOrg(context.Background(), "teamscope-org", "Teamscope Org", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	pIn, err := s.org.CreateProject(context.Background(), o.ID, "team-in", "Team In", "go")
	if err != nil {
		t.Fatalf("create project (in team): %v", err)
	}
	pOut, err := s.org.CreateProject(context.Background(), o.ID, "team-out", "Team Out", "go")
	if err != nil {
		t.Fatalf("create project (out of team): %v", err)
	}
	team, err := s.org.CreateTeam(context.Background(), o.ID, "teamscope-team", "Teamscope Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.org.AddTeamMember(context.Background(), team.ID, memberID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	// member привязан командой только к pIn — pOut остаётся вне его скоупа.
	if err := s.org.AttachTeam(context.Background(), pIn.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	resp := getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/projects", memberCookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), pIn.Name) {
		t.Errorf("member не видит проект своей команды %q: %s", pIn.Name, body)
	}
	if strings.Contains(string(body), pOut.Name) {
		t.Errorf("member видит проект чужой команды той же организации %q: %s", pOut.Name, body)
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

// TestProjectsRedirectFallsBackWhenCookieOrgInaccessible — фикс-раунд 1,
// п.3: протухшая cookie "proj" (проект существует, но в ЧУЖОЙ организации,
// где у юзера нет роли) не должна повесить редирект — h.Org.ProjectOrg
// резолвит организацию, но h.Org.Role по ней падает, и код обязан
// откатиться на OrgsOf, а не остаться с orgID чужой организации.
func TestProjectsRedirectFallsBackWhenCookieOrgInaccessible(t *testing.T) {
	s := newIssuesStack(t)
	uid, cookie := registerAndLogin(t, s, "stale-cookie-redirect@example.com")
	own := createProject(t, s, uid, "stale-own-org", "stale-own-proj")

	stranger, _ := registerAndLogin(t, s, "stale-cookie-stranger@example.com")
	foreign := createProject(t, s, stranger, "stale-foreign-org", "stale-foreign-proj")

	req, err := http.NewRequest(http.MethodGet, s.srv.URL+"/projects", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(cookie)
	// Кука указывает на реальный проект — но чужой организации.
	req.AddCookie(&http.Cookie{Name: "proj", Value: strconv.FormatInt(foreign.ID, 10)})
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	want := "/orgs/" + strconv.FormatInt(own.OrgID, 10) + "/projects"
	if got := resp.Header.Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q (протухшая кука игнорируется, фолбэк на собственную организацию)", got, want)
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
