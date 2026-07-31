package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// Сквозная проверка задачи 4 (группа «тенантность»): тот же инвариант, что
// tenancy_invariant_test.go проверяет на уровне org.Service, здесь проверяется
// на границе HTTP, с ЖИВОЙ сессией исключённого участника — она намеренно не
// инвалидируется (см. докблок RemoveMember), поэтому доступ обязан пропасть
// без повторного входа: на каждый запрос CanAccessProject ходит в базу.

// tenancyRevokeEnv — состояние, которое TestRemovedMemberLosesProjectAccessOverHTTP
// собирает и проверяет до/после удаления участника.
type tenancyRevokeEnv struct {
	orgID        int64
	projectID    string
	projectSlug  string
	issueID      string
	issueIDNum   int64
	memberUserID int64
	memberCookie *http.Cookie
	ownerCookie  *http.Cookie
	dsnKey       string
}

// setupOrgWithTeamMember строит организацию с owner'ом, участником в команде,
// проектом, привязанным к этой команде, живым DSN-ключом проекта и одной
// проблемой (issue) — минимальный набор, на котором member получает доступ
// ко всем четырём проверяемым поверхностям (issues, setup, мутация статуса,
// /projects) только через членство в команде.
//
// Стенд — issuesStack (issues_test.go), а не newStack (auth_test.go): страница
// /issues читает event.Query.Sparklines для каждой непустой выдачи (issues.go,
// sparklinesFor), и h.Events == nil, как в newStack, ронял бы её паникой, как
// только в проекте появляется хотя бы одна проблема (см. предупреждение в
// issuesStack — issues_test.go:27-29).
func setupOrgWithTeamMember(t *testing.T, s *issuesStack) tenancyRevokeEnv {
	t.Helper()
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, s.auth, "tenancy-revoke-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, s.auth, "tenancy-revoke-member@example.com")

	o, err := s.org.CreateOrg(ctx, "tenancy-revoke-co", "Tenancy Revoke Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := s.org.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	team, err := s.org.CreateTeam(ctx, o.ID, "tenancy-revoke-team", "Tenancy Revoke Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.org.AddTeamMember(ctx, team.ID, memberID); err != nil {
		t.Fatalf("add team member: %v", err)
	}

	proj, err := s.org.CreateProject(ctx, o.ID, "tenancy-revoke-proj", "Tenancy Revoke Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := s.org.AttachTeam(ctx, proj.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	key, err := s.org.CreateKey(ctx, proj.ID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Проблема нужна для проверки поверхности «мутация статуса»
	// (POST /issues/{id}/status) — заводится тем же приёмом, что и в
	// issuedetail_test.go: через issue.Service.Upsert, а не прямым INSERT.
	up, err := s.issues.Upsert(ctx, proj.ID, "tenancy-revoke-fp", "Tenancy Revoke Issue", "pkg/a.go:1", "error", "", time.Now().UTC())
	if err != nil {
		t.Fatalf("upsert issue: %v", err)
	}

	return tenancyRevokeEnv{
		orgID:        o.ID,
		projectID:    strconv.FormatInt(proj.ID, 10),
		projectSlug:  proj.Slug,
		issueID:      strconv.FormatInt(up.IssueID, 10),
		issueIDNum:   up.IssueID,
		memberUserID: memberID,
		memberCookie: memberCookie,
		ownerCookie:  ownerCookie,
		dsnKey:       key.PublicKey,
	}
}

// removeMember выполняет POST /orgs/{id}/settings/remove от имени actorCookie
// — то же двухшаговое подтверждение, что и остальные тесты пакета
// (orgsettings_test.go): confirmed=yes сразу, страница подтверждения здесь не
// нужна.
func removeMember(t *testing.T, s *issuesStack, orgID, userID int64, actorCookie *http.Cookie) {
	t.Helper()
	path := "/orgs/" + strconv.FormatInt(orgID, 10) + "/settings/remove"
	form := url.Values{"confirmed": {"yes"}, "user_id": {strconv.FormatInt(userID, 10)}}
	resp := postForm(t, s.srv, path, form, s.srv.URL, actorCookie)
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove member: POST %s = %d, want 303; body: %s", path, resp.StatusCode, body)
	}
}

// readAll читает и закрывает тело ответа — общий приём тестов пакета, здесь
// оформлен как переиспользуемый хелпер, потому что вызывается на нескольких
// шагах одного теста.
func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// TestRemovedMemberLosesProjectAccessOverHTTP — проверка того же инварианта на
// уровне запросов, с ЖИВОЙ сессией: она намеренно не инвалидируется, и доступ
// обязан пропасть без повторного входа.
//
// Проверяются именно те поверхности, которые перечислены в находке: список и
// детали проблем (GET issues), страница подключения с DSN-ключами (GET setup),
// мутация статуса проблемы (POST /issues/{id}/status), а также общий список
// проектов.
func TestRemovedMemberLosesProjectAccessOverHTTP(t *testing.T) {
	s := newIssuesStack(t)
	env := setupOrgWithTeamMember(t, s)
	statusPath := "/issues/" + env.issueID + "/status"

	for _, path := range []string{
		"/projects/" + env.projectID + "/issues",
		"/projects/" + env.projectID + "/setup",
	} {
		resp := getWithCookie(t, s.srv, path, env.memberCookie)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("до удаления GET %s = %d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// До удаления смена статуса проходит — иначе 404 после удаления ничего не
	// доказывает: он мог бы возвращаться и на заведомо неверный идентификатор.
	resp := postForm(t, s.srv, statusPath, url.Values{"status": {"resolved"}}, s.srv.URL, env.memberCookie)
	if code := statusOf(t, resp); code != http.StatusSeeOther {
		t.Fatalf("до удаления POST %s = %d, want 303", statusPath, code)
	}
	it, err := s.issues.Get(context.Background(), env.issueIDNum)
	if err != nil {
		t.Fatalf("get issue after pre-removal status change: %v", err)
	}
	if it.Status != "resolved" {
		t.Fatalf("до удаления статус не сменился: %q, want resolved", it.Status)
	}

	removeMember(t, s, env.orgID, env.memberUserID, env.ownerCookie)

	for _, path := range []string{
		"/projects/" + env.projectID + "/issues",
		"/projects/" + env.projectID + "/setup",
	} {
		resp := getWithCookie(t, s.srv, path, env.memberCookie)
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("после удаления GET %s = %d, want 404", path, resp.StatusCode)
		}
		if strings.Contains(body, env.dsnKey) {
			t.Errorf("страница %s отдала ключ приёма исключённому участнику", path)
		}
	}

	// После удаления мутация статуса той же живой cookie — 404, и статус в базе
	// не меняется (код ответа сам по себе мутацию не опровергает).
	resp = postForm(t, s.srv, statusPath, url.Values{"status": {"ignored"}}, s.srv.URL, env.memberCookie)
	if code := statusOf(t, resp); code != http.StatusNotFound {
		t.Errorf("после удаления POST %s = %d, want 404", statusPath, code)
	}
	it, err = s.issues.Get(context.Background(), env.issueIDNum)
	if err != nil {
		t.Fatalf("get issue after post-removal status attempt: %v", err)
	}
	if it.Status != "resolved" {
		t.Errorf("исключённый участник сменил статус проблемы: %q, want resolved (не изменился)", it.Status)
	}

	// Список проектов больше не показывает проект.
	resp = getWithCookie(t, s.srv, "/projects", env.memberCookie)
	if body := readAll(t, resp); strings.Contains(body, env.projectSlug) {
		t.Error("исключённый участник видит проект в списке")
	}
}
