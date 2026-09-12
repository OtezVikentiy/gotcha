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

// живая сессия исключённого участника намеренно не инвалидируется — доступ проверяется в БД на каждый запрос.
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

// issuesStack, не newStack — /issues читает event.Query.Sparklines, и h.Events==nil там уронит панику.
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

	keys, err := s.org.CreateKeys(ctx, proj.ID, org.KindServer)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	key := keys[0]

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

// confirmed=yes — тот же приём, что у остальных тестов пакета: страница подтверждения не нужна.
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

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

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

	// смена статуса до удаления обязана пройти — иначе 404 после не доказывает ничего.
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

	// проверяем и статус в БД — код ответа сам по себе не доказывает, что мутация не прошла.
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

	resp = getWithCookie(t, s.srv, "/projects", env.memberCookie)
	if body := readAll(t, resp); strings.Contains(body, env.projectSlug) {
		t.Error("исключённый участник видит проект в списке")
	}
}
