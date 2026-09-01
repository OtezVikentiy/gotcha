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
	"time"

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
// member — честный 403 (№72: членство уже известно, «не найдено» читалось бы
// как поломка), смена роли работает, last-owner защищён, self-действия
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

	// GET настроек member'ом (не owner/admin) → честный 403 (№72); не-члену
	// чужой организации по-прежнему отвечает 404 (crossorg_idor_test).
	resp = getWithCookie(t, s.srv, settingsPath, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET %s (member) status = %d, want 403", settingsPath, resp.StatusCode)
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
	resp = postForm(t, s.srv, removePath, url.Values{"confirmed": {"yes"}, "user_id": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (self-remove) status = %d, want 422: %s", removePath, resp.StatusCode, body)
	}

	// POST remove: попытка удалить единственного owner'а → 422.
	resp = postForm(t, s.srv, removePath, url.Values{"confirmed": {"yes"}, "user_id": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, memberCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (remove last owner) status = %d, want 422: %s", removePath, resp.StatusCode, body)
	}

	// POST remove: owner убирает admin'а (бывший member) → 303, участник удалён.
	resp = postForm(t, s.srv, removePath, url.Values{"confirmed": {"yes"}, "user_id": {strconv.FormatInt(memberID, 10)}}, s.srv.URL, ownerCookie)
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

	// /orgs/{id}/projects (задача 5 nav-ia, дверь взамен плоского /projects)
	// показывает owner'у кнопку создания проекта — тот же принцип, что раньше
	// был у ссылки на настройки организации: управляющая кнопка видна только
	// тому, у кого действие вообще доступно.
	if _, err := orgSvc.CreateProject(context.Background(), o.ID, "orgsettings-proj", "OrgSettings Proj", "go"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	orgProjectsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/projects"
	projectsResp := getWithCookie(t, s.srv, orgProjectsPath, ownerCookie)
	projectsBody, _ := io.ReadAll(projectsResp.Body)
	projectsResp.Body.Close()
	if !strings.Contains(string(projectsBody), `id="new-project"`) {
		t.Fatalf("GET %s missing create-project trigger: %s", orgProjectsPath, projectsBody)
	}
}

// TestWebInviteEmailMismatch — SEC-M2: POST /invite/{token} чужим (не тем, на
// кого выписан инвайт) юзером → 422 с понятным сообщением, членство не
// создаётся, инвайт остаётся действующим; правильный юзер принимает его.
func TestWebInviteEmailMismatch(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, _ := orgSettingsRegister(t, authSvc, "mm-web-owner@example.com")
	o, err := orgSvc.CreateOrg(context.Background(), "mm-web-co", "MM Web Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	token, err := orgSvc.Invite(context.Background(), o.ID, "mm-web-invited@example.com", org.RoleMember)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	invitePath := "/invite/" + token

	// Чужой юзер (другой email) → 422 с сообщением про другой email, членства нет.
	strangerID, strangerCookie := orgSettingsRegister(t, authSvc, "mm-web-stranger@example.com")
	resp := postForm(t, s.srv, invitePath, url.Values{}, s.srv.URL, strangerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (stranger) status = %d, want 422: %s", invitePath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "другой email") {
		t.Fatalf("POST %s (stranger) body missing mismatch message: %s", invitePath, body)
	}
	if _, err := orgSvc.Role(context.Background(), o.ID, strangerID); !errors.Is(err, org.ErrNotMember) {
		t.Fatalf("stranger role: got %v, want ErrNotMember", err)
	}

	// Правильный юзер принимает тот же (не потраченный) инвайт → 303, member.
	invitedID, invitedCookie := orgSettingsRegister(t, authSvc, "mm-web-invited@example.com")
	resp = postForm(t, s.srv, invitePath, url.Values{}, s.srv.URL, invitedCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (invited) status = %d, want 303", invitePath, resp.StatusCode)
	}
	if role, err := orgSvc.Role(context.Background(), o.ID, invitedID); err != nil || role != org.RoleMember {
		t.Fatalf("invited role = %v, %v, want member, nil", role, err)
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
	resp = postForm(t, s.srv, removePath, url.Values{"confirmed": {"yes"}, "user_id": {strconv.FormatInt(ownerID, 10)}}, s.srv.URL, adminCookie)
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
		settingsHref := "/projects/" + strconv.FormatInt(p.ID, 10) + "/settings"
		got := strings.Contains(string(body), settingsHref)
		if got != tc.wantLink {
			t.Fatalf("GET %s (%s): Project settings link present = %v, want %v: %s", issuesPath, tc.descriptor, got, tc.wantLink, body)
		}

		// /orgs/{id}/projects (задача 5 nav-ia, дверь взамен плоского
		// /projects): любой участник видит страницу, но кнопку создания
		// проекта — только owner/admin, тот же принцип, что раньше был у
		// ссылки на настройки организации в плоском списке.
		orgProjectsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/projects"
		projResp := getWithCookie(t, s.srv, orgProjectsPath, tc.cookie)
		projBody, _ := io.ReadAll(projResp.Body)
		projResp.Body.Close()
		if projResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (%s) status = %d, want 200: %s", orgProjectsPath, tc.descriptor, projResp.StatusCode, projBody)
		}
		gotCreate := strings.Contains(string(projBody), `id="new-project"`)
		if gotCreate != tc.wantLink {
			t.Fatalf("GET %s (%s): create-project trigger present = %v, want %v: %s", orgProjectsPath, tc.descriptor, gotCreate, tc.wantLink, projBody)
		}
	}
}

// TestWebOrgSettingsQuota — задача 5 (квота): блок «использование за месяц /
// лимит» на странице настроек, форма смены лимита (owner|admin, тот же
// requireOrgRole, что и остальные org-настройки), отрицательное значение →
// 422 (ErrInvalidQuota), 0 = безлимит.
func TestWebOrgSettingsQuota(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "quota-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "quota-member@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "quota-co", "Quota Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := orgSvc.IncUsage(context.Background(), o.ID, time.Now()); err != nil {
		t.Fatalf("inc usage: %v", err)
	}

	settingsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"
	quotaPath := settingsPath + "/quota"

	// GET показывает текущее использование (1) и лимит по умолчанию (1000000).
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "1000000") {
		t.Fatalf("GET %s missing default quota 1000000: %s", settingsPath, body)
	}

	// POST quota без Origin -> 403.
	resp = postForm(t, s.srv, quotaPath, url.Values{"event_quota": {"500"}}, "", ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (no origin) status = %d, want 403", quotaPath, resp.StatusCode)
	}

	// POST quota member -> 403 (№72).
	resp = postForm(t, s.srv, quotaPath, url.Values{"event_quota": {"500"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s (member) status = %d, want 403", quotaPath, resp.StatusCode)
	}

	// POST quota отрицательная -> 422, лимит не изменился.
	resp = postForm(t, s.srv, quotaPath, url.Values{"event_quota": {"-1"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (negative) status = %d, want 422: %s", quotaPath, resp.StatusCode, body)
	}
	if got, err := orgSvc.Get(context.Background(), o.ID); err != nil || got.EventQuota != 1_000_000 {
		t.Fatalf("quota after rejected negative POST = %+v, err=%v, want 1000000", got, err)
	}

	// POST quota валидная -> 303, лимит обновлён и виден на странице.
	resp = postForm(t, s.srv, quotaPath, url.Values{"event_quota": {"500"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d, want 303", quotaPath, resp.StatusCode)
	}
	if got, err := orgSvc.Get(context.Background(), o.ID); err != nil || got.EventQuota != 500 {
		t.Fatalf("quota after valid POST = %+v, err=%v, want 500", got, err)
	}
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "500") {
		t.Fatalf("GET %s missing updated quota 500: %s", settingsPath, body)
	}

	// POST quota 0 -> безлимит, отображается отдельным текстом.
	resp = postForm(t, s.srv, quotaPath, url.Values{"event_quota": {"0"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (zero) status = %d, want 303", quotaPath, resp.StatusCode)
	}
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(strings.ToLower(string(body)), "безлимит") {
		t.Fatalf("GET %s missing unlimited marker after quota=0: %s", settingsPath, body)
	}
}

// TestWebOrgSettingsRateGuard — задача 3c (PROD-B3): все квоты (события,
// транзакции, метрики, профили) видимы как единый защитный лимит приёма
// (rate-guard). Один POST сохраняет все 4 через соответствующие Set*Quota;
// 0 = безлимит. Проверяем, что значения долетели в БД (через Get), а страница
// показывает заголовок rate-guard и usage-строки по каждому виду.
func TestWebOrgSettingsRateGuard(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "rateguard-owner@example.com")

	o, err := orgSvc.CreateOrg(context.Background(), "rateguard-co", "RateGuard Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	settingsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"
	quotaPath := settingsPath + "/quota"

	// GET показывает заголовок rate-guard и usage-строки всех 4 видов приёма.
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	for _, marker := range []string{"rate-guard", "Транзакции", "Метрики", "Профили", "event_quota", "transaction_quota", "metric_quota", "profile_quota"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("GET %s missing rate-guard marker %q: %s", settingsPath, marker, body)
		}
	}

	// POST сохраняет все 4 квоты за один раз → 303.
	form := url.Values{
		"event_quota":       {"500"},
		"transaction_quota": {"400"},
		"metric_quota":      {"300"},
		"profile_quota":     {"200"},
	}
	resp = postForm(t, s.srv, quotaPath, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (all quotas) status = %d, want 303", quotaPath, resp.StatusCode)
	}
	got, err := orgSvc.Get(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if got.EventQuota != 500 || got.TransactionQuota != 400 || got.MetricQuota != 300 || got.ProfileQuota != 200 {
		t.Fatalf("quotas after POST = %+v, want event=500 tx=400 metric=300 profile=200", got)
	}

	// 0 = безлимит для транзакций/метрик/профилей; отрицательное значение → 422.
	resp = postForm(t, s.srv, quotaPath, url.Values{
		"event_quota": {"0"}, "transaction_quota": {"0"}, "metric_quota": {"0"}, "profile_quota": {"0"},
	}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s (zeros) status = %d, want 303", quotaPath, resp.StatusCode)
	}
	got, err = orgSvc.Get(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if got.EventQuota != 0 || got.TransactionQuota != 0 || got.MetricQuota != 0 || got.ProfileQuota != 0 {
		t.Fatalf("quotas after zero POST = %+v, want all 0", got)
	}

	// Отрицательная транзакционная квота → 422, значения не изменились.
	resp = postForm(t, s.srv, quotaPath, url.Values{"transaction_quota": {"-5"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (negative tx) status = %d, want 422", quotaPath, resp.StatusCode)
	}
	if got, err := orgSvc.Get(context.Background(), o.ID); err != nil || got.TransactionQuota != 0 {
		t.Fatalf("tx quota after rejected negative = %+v, err=%v, want 0", got, err)
	}
}

// TestWebOrgSettingsLogQuota — волна 3, задача H: квота логов управляется
// формой квот организации по образцу остальных квот rate-guard (см.
// TestWebOrgSettingsRateGuard). Сохранение идёт через org.SetLogQuota
// (отдельный вызов после атомарного SetQuotas — см. orgSettingsQuota),
// валидация и права — общие с остальными полями формы.
func TestWebOrgSettingsLogQuota(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "logquota-owner@example.com")
	memberID, memberCookie := orgSettingsRegister(t, authSvc, "logquota-member@example.com")

	o, err := orgSvc.CreateOrg(ctx, "logquota-co", "LogQuota Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, memberID, org.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Два принятых лога -> logs_count=2 (usage, независимый от events_count и
	// остальных счётчиков).
	if granted, err := orgSvc.CheckAndCountLogs(ctx, o.ID, time.Now(), 1_000_000, 2); err != nil || granted != 2 {
		t.Fatalf("seed logs usage: granted=%v err=%v, want (2,nil)", granted, err)
	}

	settingsPath := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings"
	quotaPath := settingsPath + "/quota"

	// GET показывает строку логов: заголовок вида приёма, имя поля формы и
	// текущее использование (2).
	resp := getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200: %s", settingsPath, resp.StatusCode, body)
	}
	for _, marker := range []string{"log_quota", "Логи"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("GET %s missing log quota marker %q: %s", settingsPath, marker, body)
		}
	}

	// POST log_quota member -> 403 (requireOrgRole — та же граница, что у
	// остальных квот, №72).
	resp = postForm(t, s.srv, quotaPath, url.Values{"log_quota": {"500"}}, s.srv.URL, memberCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST %s log_quota (member) status = %d, want 403", quotaPath, resp.StatusCode)
	}
	if got, err := orgSvc.Get(ctx, o.ID); err != nil || got.LogQuota != 0 {
		t.Fatalf("log quota after member POST = %+v, err=%v, want 0 (untouched)", got, err)
	}

	// POST log_quota мусор -> 422, значение не изменилось.
	resp = postForm(t, s.srv, quotaPath, url.Values{"log_quota": {"not-a-number"}}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s log_quota (garbage) status = %d, want 422: %s", quotaPath, resp.StatusCode, body)
	}
	if got, err := orgSvc.Get(ctx, o.ID); err != nil || got.LogQuota != 0 {
		t.Fatalf("log quota after garbage POST = %+v, err=%v, want 0", got, err)
	}

	// POST log_quota отрицательная -> 422, значение не изменилось.
	resp = postForm(t, s.srv, quotaPath, url.Values{"log_quota": {"-1"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s log_quota (negative) status = %d, want 422", quotaPath, resp.StatusCode)
	}
	if got, err := orgSvc.Get(ctx, o.ID); err != nil || got.LogQuota != 0 {
		t.Fatalf("log quota after negative POST = %+v, err=%v, want 0", got, err)
	}

	// POST log_quota валидная -> 303, значение доехало до хранилища и
	// читается обратно; остальные четыре квоты не тронуты формой, несущей
	// только log_quota.
	resp = postForm(t, s.srv, quotaPath, url.Values{"log_quota": {"777"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s log_quota status = %d, want 303", quotaPath, resp.StatusCode)
	}
	got, err := orgSvc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if got.LogQuota != 777 {
		t.Fatalf("LogQuota after POST = %d, want 777", got.LogQuota)
	}
	if got.EventQuota != 1_000_000 || got.TransactionQuota != 0 || got.MetricQuota != 0 || got.ProfileQuota != 0 {
		t.Fatalf("other quotas changed by log-only POST: %+v", got)
	}
	resp = getWithCookie(t, s.srv, settingsPath, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "777") {
		t.Fatalf("GET %s missing updated log quota 777: %s", settingsPath, body)
	}

	// POST log_quota вместе с event_quota -> оба применяются одним атомарным
	// UPDATE внутри org.SetQuotas (пять полей, не только исходные четыре).
	resp = postForm(t, s.srv, quotaPath, url.Values{"log_quota": {"0"}, "event_quota": {"42"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s log+event status = %d, want 303", quotaPath, resp.StatusCode)
	}
	got, err = orgSvc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if got.LogQuota != 0 || got.EventQuota != 42 {
		t.Fatalf("quotas after combined POST = %+v, want log=0 event=42", got)
	}

	// Атомарность (P2-8, теперь и для логов): валидные event/transaction/
	// metric/profile вместе с мусорным log_quota -> 422, и НИ ОДНА из пяти
	// квот не применяется (единый UPDATE в org.SetQuotas валидирует все поля
	// до записи — частичное применение с "четыре сохранились, log_quota нет"
	// было бы обманом пользователя, увидевшего 422).
	resp = postForm(t, s.srv, quotaPath, url.Values{
		"event_quota":       {"111"},
		"transaction_quota": {"222"},
		"metric_quota":      {"333"},
		"profile_quota":     {"444"},
		"log_quota":         {"not-a-number"},
	}, s.srv.URL, ownerCookie)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("POST %s (valid four + garbage log) status = %d, want 422: %s", quotaPath, resp.StatusCode, body)
	}
	got, err = orgSvc.Get(ctx, o.ID)
	if err != nil {
		t.Fatalf("get org: %v", err)
	}
	if got.EventQuota != 42 || got.TransactionQuota != 0 || got.MetricQuota != 0 || got.ProfileQuota != 0 || got.LogQuota != 0 {
		t.Fatalf("quotas after rejected mixed POST = %+v, want unchanged (event=42 tx=0 metric=0 profile=0 log=0)", got)
	}
}

func TestWebOrgSettingsSSO(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "sso-set-owner@example.com")
	adminID, adminCookie := orgSettingsRegister(t, authSvc, "sso-set-admin@example.com")
	o, err := orgSvc.CreateOrg(ctx, "sso-set-co", "SSO Set Co", ownerID)
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, o.ID, adminID, org.RoleAdmin); err != nil {
		t.Fatalf("add admin: %v", err)
	}
	// SSO настраивает только админ инстанса (requireInstanceAdminForSSO) —
	// делаем owner'а инстанс-админом для проверки happy-path сохранения.
	if _, err := s.pool.Exec(ctx, "UPDATE users SET is_instance_admin = true WHERE id = $1", ownerID); err != nil {
		t.Fatalf("promote owner to instance admin: %v", err)
	}
	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/sso"
	form := url.Values{
		"issuer": {"https://idp.example/realms/x"}, "client_id": {"cid"}, "client_secret": {"sec"},
		"domain": {"corp.com"}, "default_role": {"member"}, "enforced": {"on"},
	}

	// Owner (инстанс-админ) сохраняет SSO → 303, конфиг в БД.
	resp := postForm(t, s.srv, base, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("owner sso save status = %d, want 303", resp.StatusCode)
	}
	cfg, ok, _ := orgSvc.SSOByOrg(ctx, o.ID)
	if !ok || cfg.Domain != "corp.com" || !cfg.Enforced {
		t.Fatalf("sso not saved: %+v ok=%v", cfg, ok)
	}

	// Страница настроек (owner) показывает Redirect URI и домен.
	resp = getWithCookie(t, s.srv, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/settings", ownerCookie)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "/auth/oauth/sso-"+strconv.FormatInt(o.ID, 10)+"/callback") ||
		!strings.Contains(string(body), "corp.com") {
		t.Fatalf("settings page missing SSO redirect/domain: %s", body)
	}

	// Не-инстанс-админ (org-admin adminID) настраивать SSO не может → 403.
	resp = postForm(t, s.srv, base, form, s.srv.URL, adminCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-instance-admin sso save status = %d, want 403", resp.StatusCode)
	}

	// Инстанс-админ (ownerID) настраивает SSO любого орга; тот же домен, что уже
	// занят org o, → 422 (владеть o2 инстанс-админу не требуется).
	o2, _ := orgSvc.CreateOrg(ctx, "sso-set-co2", "SSO Set Co2", adminID)
	base2 := "/orgs/" + strconv.FormatInt(o2.ID, 10) + "/settings/sso"
	resp = postForm(t, s.srv, base2, form, s.srv.URL, ownerCookie) // тот же domain corp.com
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("domain-taken status = %d, want 422", resp.StatusCode)
	}

	// Delete (инстанс-админ) → конфиг убран.
	resp = postForm(t, s.srv, base+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sso delete status = %d, want 303", resp.StatusCode)
	}
	if _, ok, _ := orgSvc.SSOByOrg(ctx, o.ID); ok {
		t.Fatal("sso should be gone after delete")
	}
}

// TestWebOrgSettingsSSOOwnerNotInstanceAdminRejected — P1 находка ревью T4:
// requireInstanceAdminForSSO (orgsettings.go) гейтит SSO-ручки глобальным
// флагом users.is_instance_admin, а НЕ ролью владельца организации
// (lvlInstanceAdmin в authz_map_test.go, не lvlOwner). Ни один существующий
// тест не бил ровно в эту угрозу: TestWebOrgSettingsSSO проверяет отказ
// org-admin'у (adminCookie, роль RoleAdmin), а не владельцу;
// TestWebOrgSettingsSSONoteForNonAdmin проверяет только GET-рендер (скрыта
// ли форма), не сам POST-гейт. Модель угроз (docs/tech-docs/07-security-privacy.md)
// описывает защиту именно от владельца своей организации, самостоятельно
// подключающего SSO для непроверенного домена — self-service захват аккаунта.
// Этот тест — owner своей организации, НЕ являющийся инстанс-админом, получает
// 403 и на настройку, и на удаление SSO.
func TestWebOrgSettingsSSOOwnerNotInstanceAdminRejected(t *testing.T) {
	s := newStack(t)
	authSvc := auth.NewService(s.pool)
	orgSvc := org.NewService(s.pool, 1_000_000)
	ctx := context.Background()

	// Первый Register инстанса становится bootstrap instance-admin (see
	// auth.Service.Register, internal/auth/user.go:65-84, частичный уникальный
	// индекс one_instance_admin — админ инстанса ровно один). Расходуем этот
	// слот на одноразового пользователя ДО владельца теста: иначе владелец сам
	// оказался бы инстанс-админом и легитимно проходил бы SSO-ручки — не
	// разрыв гейта, а особенность порядка регистрации в тесте.
	orgSettingsRegister(t, authSvc, "sso-owner-reject-bootstrap@example.com")
	ownerID, ownerCookie := orgSettingsRegister(t, authSvc, "sso-owner-reject-owner@example.com")
	o, err := orgSvc.CreateOrg(ctx, "sso-owner-reject-co", "SSO Owner Reject Co", ownerID)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// Предусловие теста: владелец ДЕЙСТВИТЕЛЬНО owner своей организации и
	// ДЕЙСТВИТЕЛЬНО не инстанс-админ — иначе тест зелёный по неверной причине
	// (например, если bootstrap-слот случайно достался не тому пользователю).
	if role, err := orgSvc.Role(ctx, o.ID, ownerID); err != nil || role != org.RoleOwner {
		t.Fatalf("precondition: owner role in org = (%v,%v), want (RoleOwner,nil)", role, err)
	}
	if admin, err := authSvc.UserIsInstanceAdmin(ctx, ownerID); err != nil || admin {
		t.Fatalf("precondition: owner UserIsInstanceAdmin = (%v,%v), want (false,nil)", admin, err)
	}

	base := "/orgs/" + strconv.FormatInt(o.ID, 10) + "/settings/sso"
	form := url.Values{
		"issuer": {"https://idp.example/realms/x"}, "client_id": {"cid"}, "client_secret": {"sec"},
		"domain": {"owner-reject.example"}, "default_role": {"member"},
	}

	// Owner (не инстанс-админ) настраивает SSO своей же организации → 403.
	resp := postForm(t, s.srv, base, form, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("owner (non-instance-admin) sso save status = %d, want 403", resp.StatusCode)
	}
	if _, ok, _ := orgSvc.SSOByOrg(ctx, o.ID); ok {
		t.Fatal("sso must not be saved: owner is not instance admin")
	}

	// Owner (не инстанс-админ) удаляет SSO своей же организации → тоже 403,
	// даже с confirmed=yes (гейт стоит до подтверждения удаления).
	resp = postForm(t, s.srv, base+"/delete", url.Values{"confirmed": {"yes"}}, s.srv.URL, ownerCookie)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("owner (non-instance-admin) sso delete status = %d, want 403", resp.StatusCode)
	}
}
