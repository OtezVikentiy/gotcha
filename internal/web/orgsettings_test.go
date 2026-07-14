package web_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gitflic.ru/otezvikentiy/gotcha/internal/auth"
	"gitflic.ru/otezvikentiy/gotcha/internal/org"
)

// orgSettingsRegister регистрирует юзера через auth.Service напрямую (без
// HTTP) и возвращает его id + cookie сессии — тот же приём, что
// registerAndLogin в issues_test.go, но привязанный к authSvc из newStack
// (настройкам организации CH-инфраструктура issuesStack не нужна).
func orgSettingsRegister(t *testing.T, authSvc *auth.Service, email string) (int64, *http.Cookie) {
	t.Helper()
	uid, err := authSvc.Register(context.Background(), email, "correct-horse-battery")
	if err != nil {
		t.Fatalf("register %s: %v", email, err)
	}
	token, err := authSvc.CreateSession(context.Background(), uid)
	if err != nil {
		t.Fatalf("create session for %s: %v", email, err)
	}
	return uid, &http.Cookie{Name: auth.CookieName, Value: token}
}

var inviteLinkRe = regexp.MustCompile(`(http\S*/invite/\S+)</code>`)

func extractInviteLink(t *testing.T, body string) string {
	t.Helper()
	m := inviteLinkRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("invite link not found in body: %s", body)
	}
	return m[1]
}

// TestWebOrgSettings — сквозной сценарий задачи 5/2: owner видит настройки,
// member — 404, смена роли работает, last-owner защищён, self-действия
// запрещены, invite выдаёт ссылку один раз и принимается один раз.
func TestWebOrgSettings(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "orgsettings-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "orgsettings-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "orgsettings-co", "OrgSettings Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	settingsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"

	// GET настроек owner'ом → 200, оба email видны в таблице участников.
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s (owner) status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "orgsettings-owner@example.com") || !strings.Contains(string(body), "orgsettings-member@example.com") {
		t.Fatalf("GET %s (owner) missing member emails: %s", settingsPath, body)
	}

	// GET настроек member'ом (не owner/admin) → 404.
	resp = getWithCookie(t, s.srv, settingsPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET %s (member) status = %d, want 404", settingsPath, resp.StatusCode)
	}

	rolePath := settingsPath + "/role"
	removePath := settingsPath + "/remove"
	invitePath := settingsPath + "/invite"

	// POST role без Origin → 403.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"admin"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", rolePath, resp.StatusCode)
	}

	// POST role: owner меняет роль member → admin → 303, роль обновлена.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"admin"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", rolePath, resp.StatusCode)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleAdmin {
		t.Fatalf("role after change = %v, %v, want admin, nil", role, err)
	}

	// POST role себе (owner меняет свою роль) → 422.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}, "role": {"admin"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (self-change) status = %d, want 422: %s", rolePath, resp.StatusCode, body)
	}

	// POST role: попытка понизить единственного owner'а → 422 (ErrLastOwner).
	// memberCookie теперь принадлежит admin'у — тоже имеет доступ к настройкам.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}, "role": {"member"}}, s.srv.URL, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (demote last owner) status = %d, want 422: %s", rolePath, resp.StatusCode, body)
	}

	// POST remove себе → 422.
	resp = postForm(t, s.srv, removePath, url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (self-remove) status = %d, want 422: %s", removePath, resp.StatusCode, body)
	}

	// POST remove: попытка удалить единственного owner'а → 422.
	resp = postForm(t, s.srv, removePath, url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (remove last owner) status = %d, want 422: %s", removePath, resp.StatusCode, body)
	}

	// POST remove: owner убирает admin'а (бывший member) → 303, участник удалён.
	resp = postForm(t, s.srv, removePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", removePath, resp.StatusCode)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, memberID); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("role after remove: got err %v, want ErrNotMember", err)
	}

	// POST invite с невалидным email → 422.
	resp = postForm(t, s.srv, invitePath, url.Values{"email": {"not-an-email"}, "role": {"member"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (bad email) status = %d, want 422: %s", invitePath, resp.StatusCode, body)
	}

	// POST invite валидный → 200 сразу со ссылкой-приглашением (без редиректа:
	// одноразовый токен нельзя протаскивать через query/Location).
	resp = postForm(t, s.srv, invitePath, url.Values{"email": {"orgsettings-invited@example.com"}, "role": {"member"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status = %d, want 200: %s", invitePath, resp.StatusCode, body)
	}
	inviteLink := extractInviteLink(t, string(body))
	if !strings.HasPrefix(inviteLink, s.srv.URL+"/invite/") {
		t.Fatalf("invite link %q does not start with %s/invite/", inviteLink, s.srv.URL)
	}
	inviteRelPath := strings.TrimPrefix(inviteLink, s.srv.URL)

	// Второй юзер регистрируется и логинится, GET /invite/{token} → 200.
	invitedID, invitedCookie := orgSettingsRegister(t, authSvc, "orgsettings-invited@example.com")
	resp = getWithCookie(t, s.srv, inviteRelPath, invitedCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", inviteRelPath, resp.StatusCode, body)
	}

	// POST /invite/{token} → 303 /, роль == приглашённой (member).
	resp = postForm(t, s.srv, inviteRelPath, url.Values{}, s.srv.URL, invitedCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", inviteRelPath, resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/" {
		t.Fatalf("POST %s Location = %q, want /", inviteRelPath, got)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, invitedID); err != nil || role != org.RoleMember {
		t.Fatalf("invited role = %v, %v, want member, nil", role, err)
	}

	// Повторное принятие того же токена → 422.
	resp = postForm(t, s.srv, inviteRelPath, url.Values{}, s.srv.URL, invitedCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (reuse) status = %d, want 422: %s", inviteRelPath, resp.StatusCode, body)
	}

	// /projects показывает ссылку на настройки организации рядом с проектом.
	if _, err := orgSvc.CreateProject(context.Background(), o.ID, "orgsettings-proj", "OrgSettings Proj", "go"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	projectsResp := getWithCookie(t, s.srv, "/projects", ownerCookie)
	projectsBody, _ := io.ReadAll(projectsResp.Body)
	projectsResp.Body.Close()
	if !strings.Contains(string(projectsBody), settingsPath) {
		t.Fatalf("GET /projects missing org settings link %q: %s", settingsPath, projectsBody)
	}
}

// TestWebOrgSettingsOwnerOnlyManagesOwnerRole — привилегия owner-level
// действий (задача 2, security fix): admin имеет доступ к настройкам
// организации (requireOrgRole пускает owner/admin), но не может ни выдать
// роль owner, ни поменять роль/удалить существующего owner'а. Только owner
// может управлять owner-уровнем; admin по-прежнему может свободно управлять
// member/admin.
func TestWebOrgSettingsOwnerOnlyManagesOwnerRole(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "escalation-owner@example.com")
	adminID, adminCookie := orgSettingsRegister(t, authSvc, "escalation-admin@example.com")
	memberID, _ := orgSettingsRegister(t, authSvc, "escalation-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "escalation-co", "Escalation Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, adminID, org.RoleAdmin); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	rolePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/role"
	removePath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/remove"

	// admin пытается выдать member роль owner → 422, роль не изменилась.
	resp := postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"owner"}}, s.srv.URL, adminCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (admin grants owner) status = %d, want 422: %s", rolePath, resp.StatusCode, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleMember {
		t.Fatalf("member role after blocked promotion = %v, %v, want member, nil", role, err)
	}

	// admin пытается понизить существующего owner'а до admin → 422, роль не изменилась.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}, "role": {"admin"}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (admin demotes owner) status = %d, want 422: %s", rolePath, resp.StatusCode, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil || role != org.RoleOwner {
		t.Fatalf("owner role after blocked demotion = %v, %v, want owner, nil", role, err)
	}

	// admin пытается удалить существующего owner'а → 422, участник не удалён.
	resp = postForm(t, s.srv, removePath, url.Values{"user_id": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, adminCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (admin removes owner) status = %d, want 422: %s", removePath, resp.StatusCode, body)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, ownerID); err != nil || role != org.RoleOwner {
		t.Fatalf("owner role after blocked removal = %v, %v, want owner, nil", role, err)
	}

	// admin по-прежнему может управлять member/admin: member → admin → member.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"admin"}}, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (admin promotes member to admin) status = %d, want 303", rolePath, resp.StatusCode)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleAdmin {
		t.Fatalf("member role after admin->admin promotion = %v, %v, want admin, nil", role, err)
	}
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"member"}}, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (admin demotes back to member) status = %d, want 303", rolePath, resp.StatusCode)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleMember {
		t.Fatalf("member role after admin demotion = %v, %v, want member, nil", role, err)
	}

	// owner может выдать owner-роль (второй owner в организации) — guard не
	// должен мешать легитимному owner-действию.
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"owner"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner grants owner) status = %d, want 303", rolePath, resp.StatusCode)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleOwner {
		t.Fatalf("member role after owner promotion = %v, %v, want owner, nil", role, err)
	}

	// owner может понизить теперь-уже-owner'а обратно (второй owner в наличии,
	// last-owner protection не срабатывает).
	resp = postForm(t, s.srv, rolePath, url.Values{"user_id": {strconv.FormatInt(memberID, 10)}, "role": {"member"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (owner demotes owner) status = %d, want 303", rolePath, resp.StatusCode)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, memberID); err != nil || role != org.RoleMember {
		t.Fatalf("member role after owner demotion = %v, %v, want member, nil", role, err)
	}
}

// TestWebManageLinksVisibility — dead link fix (задача 5/2): «Project
// settings» (список issues) и «Org settings» (список /projects) раньше
// рендерились для любого юзера с доступом, но ведут на страницы, которые
// требуют owner/admin — member получал 404 по клику. Обе ссылки теперь
// скрыты для member и видны owner/admin.
func TestWebManageLinksVisibility(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "navlinks-owner@example.com")
	adminID, adminCookie := orgSettingsRegister(t, authSvc, "navlinks-admin@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "navlinks-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "navlinks-org", "NavLinks Org", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, adminID, org.RoleAdmin); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	p, err := orgSvc.CreateProject(context.Background(), o.ID, "navlinks-proj", "NavLinks Proj", "go")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	// member получает доступ к проекту через команду (accessCondition:
	// member видит только проекты команд, в которых состоит).
	team, err := orgSvc.CreateTeam(context.Background(), o.ID, "navlinks-team", "NavLinks Team")
	if err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := orgSvc.AddTeamMember(context.Background(), team.ID, memberID); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	if err := orgSvc.AttachTeam(context.Background(), p.ID, team.ID); err != nil {
		t.Fatalf("attach team: %v", err)
	}

	issuesPath := "/projects/" + strconv.FormatInt(p.ID, 10) + "/issues"

	for _, tc := range []struct {
		name       string
		cookie     *http.Cookie
		wantLink   bool
		descriptor string
	}{
		{"owner", ownerCookie, true, "owner"},
		{"admin", adminCookie, true, "admin"},
		{"member", memberCookie, false, "member"},
	} {
		resp := getWithCookie(t, s.srv, issuesPath, tc.cookie)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (%s) status = %d, want 200: %s", issuesPath, tc.descriptor, resp.StatusCode, body)
		}
		got := strings.Contains(string(body), "Project settings")
		if got != tc.wantLink {
			t.Fatalf("GET %s (%s): Project settings link present = %v, want %v: %s", issuesPath, tc.descriptor, got, tc.wantLink, body)
		}

		projResp := getWithCookie(t, s.srv, "/projects", tc.cookie)
		projBody, _ := io.ReadAll(projResp.Body)
		projResp.Body.Close()
		if projResp.StatusCode != http.StatusOK {
			t.Fatalf("GET /projects (%s) status = %d, want 200: %s", tc.descriptor, projResp.StatusCode, projBody)
		}
		gotOrg := strings.Contains(string(projBody), "Org settings")
		if gotOrg != tc.wantLink {
			t.Fatalf("GET /projects (%s): Org settings link present = %v, want %v: %s", tc.descriptor, gotOrg, tc.wantLink, projBody)
		}
	}
}
