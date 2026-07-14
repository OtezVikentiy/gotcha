package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// TestWebTeams — сквозной сценарий задачи 3 (команды): owner видит страницу
// команд и создаёт команду (невалидный/занятый slug → 422), member — 404
// везде, добавление участника (и 422 для не-члена организации), привязка
// проекта своей организации и 422 для чужой, отвязка, удаление участника, и
// навигационные ссылки на orgsettings/issues.
func TestWebTeams(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "teams-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "teams-member@example.com")
	outsiderID, _ := orgSettingsRegister(t, authSvc, "teams-outsider@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "teams-co", "Teams Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	other, err := orgSvc.CreateOrg(context.Background(), "teams-other-co", "Other Co", ownerID)
	if err != nil {
		t.Fatalf("create other org: %v", err)
	}
	otherProj, err := orgSvc.CreateProject(context.Background(), other.ID, "other-proj", "Other Proj", "go")
	if err != nil {
		t.Fatalf("create other project: %v", err)
	}

	teamsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/teams"

	// GET owner -> 200
	resp := getWithCookie(t, s.srv, teamsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", teamsPath, resp.StatusCode, body)
	}

	// GET member (не owner/admin) -> 404
	resp = getWithCookie(t, s.srv, teamsPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", teamsPath, resp.StatusCode)
	}

	// POST create team без Origin -> 403
	resp = postForm(t, s.srv, teamsPath, url.Values{"slug": {"backend"}, "name": {"Backend"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", teamsPath, resp.StatusCode)
	}

	// POST create team с невалидным slug -> 422
	resp = postForm(t, s.srv, teamsPath, url.Values{"slug": {"Bad Slug!"}, "name": {"Backend"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (bad slug) status = %d, want 422: %s", teamsPath, resp.StatusCode, body)
	}

	// POST create team валидный -> 303
	resp = postForm(t, s.srv, teamsPath, url.Values{"slug": {"backend"}, "name": {"Backend"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", teamsPath, resp.StatusCode)
	}

	// Дубликат slug -> 422
	resp = postForm(t, s.srv, teamsPath, url.Values{"slug": {"backend"}, "name": {"Dup"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (dup slug) status = %d, want 422: %s", teamsPath, resp.StatusCode, body)
	}

	teams, err := orgSvc.TeamsOf(context.Background(), o.ID)
	if err != nil || len(teams) != 1 {
		t.Fatalf("TeamsOf = %+v, err=%v, want 1 team", teams, err)
	}
	team := teams[0]

	membersPath := "/teams/" + strconv.FormatInt(team.ID, 10) + "/members"
	membersRemovePath := membersPath + "/remove"
	projectsPath := "/teams/" + strconv.FormatInt(team.ID, 10) + "/projects"
	projectsDetachPath := projectsPath + "/detach"

	// POST members под member (не owner/admin) -> 404
	resp = postForm(t, s.srv, membersPath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST %s (member) status = %d, want 404", membersPath, resp.StatusCode)
	}

	// POST members add: чужак (не член организации) -> 422 (ErrNotMember)
	resp = postForm(t, s.srv, membersPath, url.Values{"user_id": {strconv.FormatInt(outsiderID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (outsider) status = %d, want 422: %s", membersPath, resp.StatusCode, body)
	}

	// POST members add: валидный участник организации -> 303
	resp = postForm(t, s.srv, membersPath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", membersPath, resp.StatusCode)
	}
	tmembers, err := orgSvc.TeamMembers(context.Background(), team.ID)
	if err != nil || len(tmembers) != 1 || tmembers[0].UserID != memberID {
		t.Fatalf("TeamMembers after add = %+v err=%v, want [memberID]", tmembers, err)
	}

	// Привязка проекта своей организации -> 303
	proj, err := orgSvc.CreateProject(context.Background(), o.ID, "api", "API", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	resp = postForm(t, s.srv, projectsPath, url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", projectsPath, resp.StatusCode)
	}
	tprojects, err := orgSvc.TeamProjects(context.Background(), team.ID)
	if err != nil || len(tprojects) != 1 || tprojects[0].ID != proj.ID {
		t.Fatalf("TeamProjects after attach = %+v err=%v, want [proj]", tprojects, err)
	}

	// Привязка проекта ЧУЖОЙ организации -> 422, состав не изменился
	resp = postForm(t, s.srv, projectsPath, url.Values{"project_id": {strconv.FormatInt(otherProj.ID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (cross-org project) status = %d, want 422: %s", projectsPath, resp.StatusCode, body)
	}
	tprojects, err = orgSvc.TeamProjects(context.Background(), team.ID)
	if err != nil || len(tprojects) != 1 {
		t.Fatalf("TeamProjects after cross-org attempt = %+v err=%v, want still just [proj]", tprojects, err)
	}

	// Отвязка проекта -> 303
	resp = postForm(t, s.srv, projectsDetachPath, url.Values{"project_id": {strconv.FormatInt(proj.ID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", projectsDetachPath, resp.StatusCode)
	}
	tprojects, err = orgSvc.TeamProjects(context.Background(), team.ID)
	if err != nil || len(tprojects) != 0 {
		t.Fatalf("TeamProjects after detach = %+v err=%v, want empty", tprojects, err)
	}

	// Удаление участника -> 303
	resp = postForm(t, s.srv, membersRemovePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", membersRemovePath, resp.StatusCode)
	}
	tmembers, err = orgSvc.TeamMembers(context.Background(), team.ID)
	if err != nil || len(tmembers) != 0 {
		t.Fatalf("TeamMembers after remove = %+v err=%v, want empty", tmembers, err)
	}

	// GET /orgs/{id}/settings показывает ссылку на команды.
	settingsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), teamsPath) {
		t.Fatalf("GET %s missing teams link %q: %s", settingsPath, teamsPath, body)
	}

	// GET /projects/{id}/issues показывает ссылку на настройки проекта.
	issuesPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/issues"
	projSettingsPath := "/projects/" + strconv.FormatInt(proj.ID, 10) + "/settings"
	resp = getWithCookie(t, s.srv, issuesPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), projSettingsPath) {
		t.Fatalf("GET %s missing project settings link %q: %s", issuesPath, projSettingsPath, body)
	}
}
